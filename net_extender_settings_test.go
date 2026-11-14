// The extender protocol's limits and a client's treatment of a 429 are settings
// objects (QD-702): every check reads the instance it is handed or its owner
// carries, and the defaults are A3's and A12's. So are the directory's counts
// and the alt dialers' priorities, which the same change moved.

package connect

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The defaults are the protocol's values, and a ConnectSettings that names none
// takes them.
func TestExtenderSettingsDefaultsAreTheProtocols(t *testing.T) {
	AssertEqual(t, DefaultExtenderFormatSettings().MaxHeaderByteCount, 1024)
	AssertEqual(t, DefaultExtenderLimitedSettings().RetryAfterMax, 24*time.Hour)
	AssertEqual(t, DefaultExtenderLimitedSettings().BackoffJitterFraction, 0.5)

	connectSettings := DefaultConnectSettings()
	AssertEqual(t, connectSettings.ExtenderFormatSettings.MaxHeaderByteCount, 1024)
	AssertEqual(t, connectSettings.ExtenderLimitedSettings.RetryAfterMax, 24*time.Hour)
	AssertEqual(t, DefaultExtenderDirectorySettings().ExtenderLimitedSettings.BackoffJitterFraction, 0.5)

	// a fresh object each call, so tuning one owner's tunes no other's
	if DefaultExtenderFormatSettings() == DefaultExtenderFormatSettings() {
		t.Fatal("the format defaults are one shared object")
	}
	if DefaultExtenderLimitedSettings() == DefaultExtenderLimitedSettings() {
		t.Fatal("the limited defaults are one shared object")
	}

	// settings built without the constructor read the defaults, and an owner's
	// own object is the one read
	bare := &ConnectSettings{}
	AssertEqual(t, bare.extenderFormatSettings().MaxHeaderByteCount, 1024)
	AssertEqual(t, bare.extenderLimitedSettings().RetryAfterMax, 24*time.Hour)
	AssertEqual(t, (&ExtenderDirectorySettings{}).extenderLimitedSettings().BackoffJitterFraction, 0.5)
	own := &ExtenderFormatSettings{MaxHeaderByteCount: 7}
	if (&ConnectSettings{ExtenderFormatSettings: own}).extenderFormatSettings() != own {
		t.Fatal("a client's own format settings were not the ones read")
	}
}

// A header limit below A3's bounds every frame and header built or read by it:
// the request header, the response frame, both probe frames and the tcp
// response head.
func TestExtenderFormatSettingsBoundEveryFrame(t *testing.T) {
	small := &ExtenderFormatSettings{MaxHeaderByteCount: 16}
	defaults := DefaultExtenderFormatSettings()

	extenderConfig := &ExtenderConfig{
		Profile: ExtenderProfile{ConnectMode: ExtenderConnectModeTcpTls, Port: 443},
		Ip:      netip.MustParseAddr("192.0.2.1"),
	}
	dial := &ExtenderDial{DestinationHost: "a-destination-name.example", DestinationPort: 443}
	if headerBytes, err := extenderRequestHeaderBytes(small, extenderConfig, dial); err == nil {
		t.Errorf("a %d byte header passed a 16 byte limit", len(headerBytes))
	}
	if _, err := extenderRequestHeaderBytes(defaults, extenderConfig, dial); err != nil {
		t.Fatal(err)
	}

	response := &protocol.ExtenderResponse{PublicKey: make([]byte, 32)}
	if _, err := ExtenderResponseFrame(small, response); err == nil {
		t.Error("a response over the limit was framed")
	}
	frameBytes, err := ExtenderResponseFrame(defaults, response)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadExtenderResponseFrame(small, bytes.NewReader(frameBytes)); err == nil {
		t.Error("a response frame over the limit was read")
	}
	if _, err := ReadExtenderResponseFrame(defaults, bytes.NewReader(frameBytes)); err != nil {
		t.Fatal(err)
	}

	verdict := &protocol.ExtenderProbeVerdict{Accepted: true, Cosignature: make([]byte, 64)}
	if _, err := ExtenderProbeVerdictFrame(small, verdict); err == nil {
		t.Error("a verdict over the limit was framed")
	}
	verdictBytes, err := ExtenderProbeVerdictFrame(defaults, verdict)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadExtenderProbeVerdictFrame(small, bytes.NewReader(verdictBytes)); err == nil {
		t.Error("a verdict frame over the limit was read")
	}
	attestation := &protocol.ExtenderProbeAttestation{ProbeNonce: make([]byte, 32), Signature: make([]byte, 64)}
	if _, err := ExtenderProbeAttestationFrame(small, attestation); err == nil {
		t.Error("an attestation over the limit was framed")
	}
	attestationBytes, err := ExtenderProbeAttestationFrame(defaults, attestation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadExtenderProbeAttestationFrame(small, bytes.NewReader(attestationBytes)); err == nil {
		t.Error("an attestation frame over the limit was read")
	}

	// the tcp response head reads the limit and no more, and says which limit
	source := strings.NewReader(strings.Repeat("a", 64))
	headBytes, err := io.ReadAll(newExtenderResponseHeaderReader(source, small))
	if err == nil || !strings.Contains(err.Error(), "exceed 16 bytes") {
		t.Errorf("a head over the limit read as %v", err)
	}
	AssertEqual(t, len(headBytes), 16)
	AssertEqual(t, source.Len(), 48)
}

// A client dial checks its header against its own ConnectSettings' limit, before
// any carrier is dialed: a limit the header does not fit refuses the dial with
// nothing sent.
func TestExtenderDialReadsItsFormatSettings(t *testing.T) {
	connectSettings := DefaultConnectSettings()
	connectSettings.ExtenderFormatSettings = &ExtenderFormatSettings{MaxHeaderByteCount: 4}
	dialCount := 0
	connectSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			dialCount += 1
			return nil, io.EOF
		},
	}
	for _, connectMode := range []ExtenderConnectMode{ExtenderConnectModeTcpTls, ExtenderConnectModeQuic, ExtenderConnectModeDns} {
		_, _, err := DialExtender(
			context.Background(),
			connectSettings,
			&ExtenderConfig{
				Profile: ExtenderProfile{ConnectMode: connectMode, Port: 443},
				Ip:      netip.MustParseAddr("192.0.2.1"),
			},
			&ExtenderDial{DestinationHost: "dest.example", DestinationPort: 443},
		)
		if err == nil || !strings.Contains(err.Error(), "at most 4") {
			t.Errorf("%s: a header over the client's limit dialed: %v", connectMode, err)
		}
	}
	AssertEqual(t, dialCount, 0)
}

// The Retry-After a client believes is bounded by its settings, in both of RFC
// 9110's forms, and a bound of zero believes none.
func TestExtenderRetryAfterTakesItsBoundFromSettings(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	header := http.Header{}
	minute := &ExtenderLimitedSettings{RetryAfterMax: time.Minute}
	defaults := DefaultExtenderLimitedSettings()

	header.Set("Retry-After", "3600")
	AssertEqual(t, extenderRetryAfter(minute, header, now), time.Minute)
	AssertEqual(t, extenderRetryAfter(defaults, header, now), time.Hour)
	header.Set("Retry-After", now.Add(time.Hour).Format(http.TimeFormat))
	AssertEqual(t, extenderRetryAfter(minute, header, now), time.Minute)
	AssertEqual(t, extenderRetryAfter(defaults, header, now), time.Hour)

	// a bound of part seconds never lets the seconds form past it
	header.Set("Retry-After", "10")
	AssertEqual(t, extenderRetryAfter(&ExtenderLimitedSettings{RetryAfterMax: 1500 * time.Millisecond}, header, now), time.Second)
	// a hostile count of seconds is bounded without wrapping
	header.Set("Retry-After", "9223372036854775807")
	AssertEqual(t, extenderRetryAfter(defaults, header, now), 24*time.Hour)
	// no bound believes none
	header.Set("Retry-After", "30")
	AssertEqual(t, extenderRetryAfter(&ExtenderLimitedSettings{}, header, now), time.Duration(0))
	AssertEqual(t, extenderRetryAfter(&ExtenderLimitedSettings{RetryAfterMax: -time.Second}, header, now), time.Duration(0))
}

// The jitter of a limited backoff is the settings' fraction either way of it:
// none jitters nothing, a fraction past one is taken as one, and a huge backoff
// does not wrap.
func TestExtenderLimitedBackoffJitterFollowsSettings(t *testing.T) {
	for _, fraction := range []float64{0, -0.5} {
		settings := &ExtenderLimitedSettings{BackoffJitterFraction: fraction}
		AssertEqual(t, settings.JitterBackoff(30*time.Second, time.Hour), 30*time.Second)
		AssertEqual(t, settings.JitterBackoff(0, 10*time.Second), 10*time.Second)
		AssertEqual(t, settings.JitterBackoff(0, 0), time.Duration(0))
	}

	cases := []struct {
		fraction float64
		low      time.Duration
		high     time.Duration
	}{
		{fraction: 0.25, low: 30 * time.Second, high: 50 * time.Second},
		{fraction: 1, low: 0, high: 80 * time.Second},
		{fraction: 4, low: 0, high: 80 * time.Second},
	}
	for _, c := range cases {
		settings := &ExtenderLimitedSettings{BackoffJitterFraction: c.fraction}
		drawLow, drawHigh := time.Duration(math.MaxInt64), time.Duration(0)
		for i := 0; i < 2000; i += 1 {
			backoff := settings.JitterBackoff(40*time.Second, time.Hour)
			if backoff < c.low || c.high < backoff {
				t.Fatalf("fraction %v drew %s, outside %s to %s", c.fraction, backoff, c.low, c.high)
			}
			drawLow = min(drawLow, backoff)
			drawHigh = max(drawHigh, backoff)
		}
		// the draws spread over the window rather than sitting at its middle
		if (c.high-c.low)/2 > drawHigh-drawLow {
			t.Errorf("fraction %v: 2000 draws spanned only %s to %s", c.fraction, drawLow, drawHigh)
		}
	}

	huge := DefaultExtenderLimitedSettings().JitterBackoff(time.Duration(math.MaxInt64), 0)
	if huge < time.Duration(math.MaxInt64)/2-1 {
		t.Fatalf("a huge backoff wrapped to %s", huge)
	}
}

// A directory caps its event ring and a consumer's buffer at its own settings'
// counts.
func TestExtenderDirectoryReadsItsCounts(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
		settings.EventWindowTimeout = time.Hour
		settings.MaxAddressCount = 0
		settings.EventRingCount = 8
		settings.SubscribeBufferCount = 2
	})
	messages, unsubscribe := directory.Subscribe()
	defer unsubscribe()
	AssertEqual(t, cap(messages), 2)
	now := clock.Now()
	apply := func(i int) {
		record := signTestRecord(
			t,
			rootPrivateKey,
			newTestExtenderKey(t),
			now.Add(time.Duration(i)*time.Millisecond),
			now.Add(time.Hour),
			testExtenderAddress(fmt.Sprintf("192.0.2.%d", i)),
		)
		if _, err := directory.ApplyRecord(record, ExtenderSourceGossip); err != nil {
			t.Fatal(err)
		}
	}
	// two fit the consumer's buffer while it reads nothing
	apply(0)
	apply(1)
	AssertEqual(t, len(messages), 2)
	// a third cuts it off, its queue voided
	apply(2)
	_, ok := <-messages
	AssertEqual(t, ok, false)

	for i := 3; i < 12; i += 1 {
		apply(i)
	}
	AssertEqual(t, directory.EventCountSince(now.Add(-time.Hour)), 8)
}

// The alt dialers, whose QUIC socket is the extender carrier's, take their
// priorities and minimum weight from the strategy's settings, L4's by default.
func TestAltDialersTakeTheirPrioritiesFromSettings(t *testing.T) {
	settings := DefaultClientStrategySettings()
	AssertEqual(t, settings.AltH3DialerPriority, 60)
	AssertEqual(t, settings.AltWhodisDialerPriority, 130)
	AssertEqual(t, settings.AltDialerMinimumWeight, float32(0.25))

	settings.AltUrl = "https://alt.example"
	settings.AltH3DialerPriority = 61
	settings.AltWhodisDialerPriority = 131
	settings.AltDialerMinimumWeight = 0.5
	dialers := newAltDialers(nil, settings)
	AssertEqual(t, len(dialers), 2)
	AssertEqual(t, dialers[0].description, "alt h3")
	AssertEqual(t, dialers[0].priority, 61)
	AssertEqual(t, dialers[1].description, "alt whodis")
	AssertEqual(t, dialers[1].priority, 131)
	for _, dialer := range dialers {
		AssertEqual(t, dialer.minimumWeight, float32(0.5))
	}
}
