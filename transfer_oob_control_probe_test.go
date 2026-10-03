package connect

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

// The actual strategy must put the telemetry marker only on owned control
// POSTs, including cleanup on a live caller context after API cancellation.
func TestApiOutOfBandControlProbeMarkerStaysOnControlPosts(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		type observedRequest struct{ path, marker, authorization string }
		var stateLock sync.Mutex
		requests := []observedRequest{}
		api, strategy := authObservationTestApi(t.Context(), nil, false, serialTestRoundTripper(func(req *http.Request) (*http.Response, error) {
			stateLock.Lock()
			requests = append(requests, observedRequest{req.URL.Path, req.Header.Get("X-Ur-Control-Probe"), req.Header.Get("Authorization")})
			stateLock.Unlock()
			authObservationTestWrote(req)
			return authObservationTestResponse(req, http.StatusOK, `{"pack":""}`), nil
		}))
		defer api.Close()
		marked := newApiOutOfBandControl(t.Context(), strategy, "synthetic-probe-token", "https://api.control.example", true)
		plain := NewApiOutOfBandControl(t.Context(), strategy, "synthetic-plain-token", "https://api.control.example")
		defer marked.CloseAndWait(t.Context())
		defer plain.CloseAndWait(t.Context())
		send := func(control *ApiOutOfBandControl, cleanup bool) {
			done := make(chan error, 1)
			callback := func(_ []*protocol.Frame, err error) { done <- err }
			if cleanup {
				control.api.Close()
				control.SendControlWithCtx(context.Background(), nil, callback)
			} else {
				control.SendControl(nil, callback)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
		send(marked, false)
		send(plain, false)
		send(marked, true)
		send(plain, true)
		if _, err := HttpPostWithStrategyRaw(t.Context(), strategy, "https://api.control.example/synthetic-generic", []byte(`{}`), "synthetic-other-token"); err != nil {
			t.Fatal(err)
		}
		if _, err := HttpGetWithStrategyRaw(t.Context(), strategy, "https://sampled-site.example/randomized-page", ""); err != nil {
			t.Fatal(err)
		}
		stateLock.Lock()
		defer stateLock.Unlock()
		markedPosts, plainPosts, hellos, generic, sampled := 0, 0, 0, 0, 0
		for _, request := range requests {
			if request.path == "/connect/control" && request.authorization == "Bearer synthetic-probe-token" {
				markedPosts++
				if request.marker != "1" {
					t.Errorf("probe control marker = %q, want 1", request.marker)
				}
				continue
			}
			if request.marker != "" {
				t.Errorf("probe marker escaped into unmarked %s", request.path)
			}
			switch request.path {
			case "/connect/control":
				plainPosts++
			case "/hello":
				hellos++
			case "/synthetic-generic":
				generic++
			case "/randomized-page":
				sampled++
			}
		}
		if markedPosts != 2 || plainPosts != 2 || hellos == 0 || generic != 1 || sampled != 1 {
			t.Fatalf("fixture coverage marked=%d plain=%d hello=%d generic=%d sampled=%d", markedPosts, plainPosts, hellos, generic, sampled)
		}
	})
}

// Each cloned retry carries the marker, but discovery attempts still do not.
// A failed transport attempt is not a completed logical OOB operation.
func TestApiOutOfBandControlProbeMarkerSurvivesRetries(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		var stateLock sync.Mutex
		posts := 0
		api, strategy := authObservationTestApi(t.Context(), nil, true, serialTestRoundTripper(func(req *http.Request) (*http.Response, error) {
			authObservationTestWrote(req)
			if req.URL.Path == "/hello" {
				if req.Header.Get(ControlProbeTelemetryHeader) != "" {
					t.Error("retry discovery inherited the control marker")
				}
				return authObservationTestResponse(req, http.StatusOK, ""), nil
			}
			stateLock.Lock()
			posts++
			attempt := posts
			stateLock.Unlock()
			if req.Header.Get(ControlProbeTelemetryHeader) != "1" || req.Header.Get("Authorization") != "Bearer synthetic-current-token" {
				t.Error("control retry lost its marker or refreshed credential")
			}
			if attempt == 1 {
				return nil, errors.New("synthetic transport failure")
			}
			return authObservationTestResponse(req, http.StatusOK, `{"pack":""}`), nil
		}))
		defer api.Close()
		control := newApiOutOfBandControl(t.Context(), strategy, "synthetic-old-token", "https://api.control.example", true)
		defer control.CloseAndWait(t.Context())
		control.SetByJwt("synthetic-current-token")
		done := make(chan error, 1)
		control.SendControl(nil, func(_ []*protocol.Frame, err error) { done <- err })
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		stateLock.Lock()
		defer stateLock.Unlock()
		if posts != 2 {
			t.Fatalf("control POST attempts = %d, want 2", posts)
		}
	})
}

// Caller mutation and another generator sharing the same strategy must not
// relabel an existing probe owner or opt ordinary SDK work into the marker.
func TestApiMultiClientControlTelemetryCapturedPerGenerator(t *testing.T) {
	api, strategy := authObservationTestApi(t.Context(), nil, true, serialTestRoundTripper(func(req *http.Request) (*http.Response, error) {
		t.Error("construction unexpectedly sent a request")
		return authObservationTestResponse(req, http.StatusOK, `{"pack":""}`), nil
	}))
	defer api.Close()
	settings := DefaultApiMultiClientGeneratorSettings()
	settings.ControlTelemetryProbe = true
	makeGenerator := func() *ApiMultiClientGenerator {
		return NewApiMultiClientGenerator(t.Context(), nil, strategy, nil,
			"https://api.control.example", "synthetic-parent", "wss://platform.control.example",
			"synthetic", "synthetic", "test", nil, DefaultClientSettings, settings)
	}
	marked := makeGenerator()
	defer marked.CloseAndWait(t.Context())
	settings.ControlTelemetryProbe = false
	plain := makeGenerator()
	defer plain.CloseAndWait(t.Context())
	markedControl := marked.newClientOob(t.Context(), "synthetic-derived")
	plainControl := plain.newClientOob(t.Context(), "synthetic-derived")
	defer markedControl.CloseAndWait(t.Context())
	defer plainControl.CloseAndWait(t.Context())
	if !markedControl.probeClaimed || plainControl.probeClaimed {
		t.Fatal("mutable/shared settings changed the per-generator marker")
	}
	sharedApiControl := NewApiOutOfBandControlWithApi(api)
	defer sharedApiControl.CloseAndWait(t.Context())
	if sharedApiControl.probeClaimed {
		t.Fatal("caller-owned shared API implicitly opted in")
	}
}
