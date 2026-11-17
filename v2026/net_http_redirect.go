package connect

// An operation can pin its exact mutation endpoint without changing the shared
// client's redirect policy for any other request or dialer generation.

import (
	"context"
	"net/http"
)

type httpRedirectsDisabledKey struct{}

// Disable redirects only for requests derived from this context. Retries keep
// the original URL, method, body and headers; they cannot allocate at a new URL.
func WithHttpRedirectsDisabled(ctx context.Context) context.Context {
	return context.WithValue(ctx, httpRedirectsDisabledKey{}, true)
}

func httpRedirectsDisabled(request *http.Request) bool {
	disabled, _ := request.Context().Value(httpRedirectsDisabledKey{}).(bool)
	return disabled
}

// A shallow per-request client copy retains the transport, jar and timeout;
// changing the cached client's CheckRedirect would race unrelated operations.
func httpClientForRequest(client *http.Client, request *http.Request) *http.Client {
	if !httpRedirectsDisabled(request) {
		return client
	}
	owned := *client
	owned.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &owned
}

// Browser fetch follows redirects inside RoundTrip, before Client can inspect
// them. Go's js transport consumes this option instead of sending it as a wire
// header. Clone first because that transport deletes its internal option.
func httpBrowserRequestForRedirectPolicy(request *http.Request) *http.Request {
	if !httpRedirectsDisabled(request) {
		return request
	}
	owned := request.Clone(request.Context())
	if owned.Header == nil {
		owned.Header = make(http.Header)
	}
	owned.Header.Set("js.fetch:redirect", "error")
	return owned
}
