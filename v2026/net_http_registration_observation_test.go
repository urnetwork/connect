// Request-scoped registration policy and auth observations share serial HTTP
// attempt boundaries. Keep both contracts through redirects and exhaustion.
package connect

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
)

// Completed redirects still reach the auth status and body-phase observation
// while the redirect target remains untouched.
func TestHttpRegistrationRedirectRetainsAuthObservation(t *testing.T) {
	counts := &AuthNetworkClientObservations{}
	ctx := WithHttpRedirectsDisabled(t.Context())
	attempts := 0
	api, _ := authObservationTestApi(ctx, counts, true, serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
		attempts++
		if request.URL.Path == "/unexpected-registration" {
			return authObservationTestResponse(request, http.StatusOK, `{"by_client_jwt":"synthetic-redirect-token"}`), nil
		}
		response := authObservationTestResponse(request, http.StatusTemporaryRedirect, "")
		response.Header.Set("Location", "/unexpected-registration")
		return response, nil
	}))
	defer api.Close()
	_, err := api.AuthNetworkClientSync(&AuthNetworkClientArgs{})
	var status *HttpStatusError
	if !errors.As(err, &status) || status.StatusCode != http.StatusTemporaryRedirect || attempts != 1 {
		t.Fatalf("scoped auth followed or lost its completed redirect: attempts=%d error=%v", attempts, err)
	}
	requireAuthObservation(t, counts, "body_read", "http_error")
}

// A current cancellation wins over a retained physical deadline for diagnostic
// classification, while callers can still inspect every original cause.
func TestHttpRegistrationExhaustionRetainsAuthCancellation(t *testing.T) {
	counts := &AuthNetworkClientObservations{}
	ctx, cancel := context.WithCancel(WithHttpRedirectsDisabled(t.Context()))
	defer cancel()
	api, _ := authObservationTestApi(ctx, counts, true, serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
		authObservationTestWrote(request)
		cancel()
		return nil, errors.Join(context.DeadlineExceeded, io.ErrUnexpectedEOF)
	}))
	defer api.Close()
	_, err := api.AuthNetworkClientSync(&AuthNetworkClientArgs{})
	var exhausted *HttpRequestExhaustedError
	if !errors.As(err, &exhausted) || !errors.Is(err, context.Canceled) || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("auth exhaustion lost its cancellation or physical causes: %v", err)
	}
	requireAuthObservation(t, counts, "response_wait", "canceled")
}
