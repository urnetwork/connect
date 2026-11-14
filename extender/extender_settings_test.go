// The extender's limits read from its settings (QD-702): the header limit it
// shares with its clients, how its NLayer front takes a hop's 429, its probe
// limiter's cap and its relay buffer.

package extender

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// A header within A3's limit but past the extender's own is refused before it
// is decoded: the limit is the instance's, not a constant.
func TestExtenderRefusesAHeaderPastItsOwnFormatLimit(t *testing.T) {
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.ExtenderFormatSettings = &connect.ExtenderFormatSettings{MaxHeaderByteCount: 16}
	})
	client := newRawExtenderHttpClient(fixture, nil)
	defer client.CloseIdleConnections()

	headerBytes := make([]byte, 17)
	response, err := client.Post("https://"+testServerName+"/", connect.ExtenderContentType, bytes.NewReader(headerBytes))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, expected %d", response.StatusCode, http.StatusForbidden)
	}
	extenderErr, ok := fixture.nextError()
	if !ok || !strings.Contains(extenderErr.Error(), "header decode") {
		t.Fatalf("attributed error = %v, expected the header stage", extenderErr)
	}

	// an extender that names none takes A3's, which refuses past 1024 bytes
	if limit := (&ExtenderSettings{}).extenderFormatSettings().MaxHeaderByteCount; limit != 1024 {
		t.Fatalf("settings that name no limit take %d", limit)
	}
	request := &http.Request{ContentLength: 1025, Body: io.NopCloser(bytes.NewReader(make([]byte, 1025)))}
	if _, err := readExtenderHeader(request, DefaultExtenderSettings().extenderFormatSettings()); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("A3's limit took a 1025 byte header: %v", err)
	}
}

// An NLayer front's hop dials speak the protocol the front serves and believe a
// hop's Retry-After as the front's own settings say: the one objects, not
// copies of the defaults.
func TestExtenderNLayerHopDialsCarryTheFrontsSettings(t *testing.T) {
	settings := DefaultExtenderSettings()
	settings.ExtenderFormatSettings = &connect.ExtenderFormatSettings{MaxHeaderByteCount: 512}
	settings.ExtenderLimitedSettings = &connect.ExtenderLimitedSettings{RetryAfterMax: time.Minute, BackoffJitterFraction: 0}
	settings.NLayerLimitedBackoff = 7 * time.Second
	server := &ExtenderServer{settings: settings}
	connectSettings := server.newNLayerConnectSettings()
	if connectSettings.ExtenderFormatSettings != settings.ExtenderFormatSettings {
		t.Error("a hop dial does not carry the front's format settings")
	}
	if connectSettings.ExtenderLimitedSettings != settings.ExtenderLimitedSettings {
		t.Error("a hop dial does not carry the front's limited settings")
	}

	// with no jitter, a limited hop is left alone for exactly its backoff
	server.nlayerHops = newExtenderNLayerHops([]*connect.ExtenderConfig{{}})
	before := time.Now()
	server.limitNLayerHop(0, 0)
	limitedUntil := server.nlayerHops[0].limitedUntil
	if limitedUntil.Before(before.Add(7*time.Second)) || time.Now().Add(7*time.Second).Before(limitedUntil) {
		t.Fatalf("a hop limited for 7s with no jitter is held until %s from %s", limitedUntil, before)
	}
}

// The probe limiter keeps a bucket for at most its settings' sources before it
// prunes the ones full again, and keeps them all under a larger cap.
func TestExtenderProbeLimiterPrunesAtItsSettingsCap(t *testing.T) {
	now := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		maxSourceCount int
		expected       int
	}{
		{maxSourceCount: 2, expected: 1},
		{maxSourceCount: DefaultExtenderSettings().ProbeMaxSourceCount, expected: 3},
	} {
		limiter := newExtenderProbeLimiter(c.maxSourceCount)
		limiter.admit("192.0.2.1", now, 1, 1)
		limiter.admit("192.0.2.2", now, 1, 1)
		// the first two are full again by the time the third arrives
		limiter.admit("192.0.2.3", now.Add(10*time.Second), 1, 1)
		if len(limiter.sourceBuckets) != c.expected {
			t.Errorf("a cap of %d kept %d sources, expected %d", c.maxSourceCount, len(limiter.sourceBuckets), c.expected)
		}
	}
}

// A forward relays intact through a relay buffer far smaller than its records,
// and through one byte at a time when the buffer is not positive.
func TestExtenderRelaysThroughItsSettingsBuffer(t *testing.T) {
	for _, relayBufferByteCount := range []int{3, 0} {
		fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
			settings.RelayBufferByteCount = relayBufferByteCount
		})
		client := connect.NewExtenderHttpClient(fixture.connectSettings(), fixture.extenderConfig(connect.ExtenderCarrierTcp))
		response, err := client.Get("https://dest.example/hello")
		if err != nil {
			t.Fatalf("a %d byte relay buffer: %v", relayBufferByteCount, err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		client.CloseIdleConnections()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "dest.example") {
			t.Fatalf("a %d byte relay buffer: status %d, body %q", relayBufferByteCount, response.StatusCode, body)
		}
	}
}
