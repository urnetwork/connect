//go:build js

package connect

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// Under js/wasm there are no sockets. Go's net/http reaches the browser's
// fetch only when the transport carries no custom dialer, and every dialer
// strategy here (ClientHello shaping, egress-bound sockets, in-process name
// resolution) is a custom dialer, so a strategy request built the native way
// silently falls back to a socket round trip that can never complete: the
// call hangs in its retry loop and the caller sees a request that was never
// sent. Each strategy request goes out as ONE fetch instead; the browser owns
// TLS, DNS and the connection pool.
//
// The fetch is bounded by RequestTimeout, as each native route is. A deadline
// before the response aborts the fetch (net/http's AbortController); a
// deadline during the body read cancels the body stream. Either way the error
// is context.DeadlineExceeded, a caller cancel is context.Canceled, and a
// fetch rejection keeps the browser's message ("net/http: fetch() failed:
// TypeError: ..."), so callers can tell a timeout from a network failure.
func (self *ClientStrategy) httpPlatformDirect(request *http.Request) (*httpResult, bool, error) {
	ctx := request.Context()
	if 0 < self.settings.RequestTimeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, self.settings.RequestTimeout)
		defer cancel()
		request = request.WithContext(ctx)
	}
	client := &http.Client{Transport: platformDirectHttpTransport()}
	response, err := httpClientForRequest(client, request).Do(request)
	if self.log.V(2).Enabled() {
		if err != nil {
			self.log.Infof("[net]http fetch %s %s = %s\n", request.Method, request.URL, err)
		} else {
			self.log.Infof("[net]http fetch %s %s = %s\n", request.Method, request.URL, response.Status)
		}
	}
	// reports whether the body stream was canceled before the read finished
	stopBodyCancel := func() bool { return true }
	if err == nil && response.Body != nil {
		// net/http aborts the fetch only until the response arrives; a body
		// that stalls after that is ended by canceling its stream
		body := response.Body
		stopBodyCancel = context.AfterFunc(ctx, func() {
			body.Close()
		})
	}
	result := newEvalResultFromHttpResponse(response, err, self.settings.MaxHttpResponseBodyBytes)
	// the one response is the selected response: read its body now, the way
	// parallelEval does for the winning route
	result.Selected()
	if !stopBodyCancel() && result.err == nil {
		// a canceled body stream reads as a clean end; the body may be partial
		result.err = ctx.Err()
	}
	httpResult, resultErr := materializeHttpResult(result)
	if resultErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(resultErr, ctxErr) {
			// the fetch or body failed because the deadline or caller ended it
			resultErr = fmt.Errorf("%w: %w", ctxErr, resultErr)
		}
		println("[net]http fetch", request.Method, request.URL.String(), "=", resultErr.Error())
		return nil, true, resultErr
	}
	return httpResult, true, nil
}

// Go's js/wasm Transport starts fetch before inspecting Context.Done. A worker
// awakened by teardown can therefore start another browser request with an
// already-canceled context. WebKit rejects that request during navigation as
// an access-control page error, even though the fetch rejection is handled.
// Check before crossing into the browser, including the streaming HTTP path.
type browserHttpTransport struct {
	http.Transport
}

func (self *browserHttpTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	return self.Transport.RoundTrip(httpBrowserRequestForRedirectPolicy(request))
}

// The one transport the browser can drive: no dialers, so net/http uses fetch.
func platformDirectHttpTransport() http.RoundTripper {
	return &browserHttpTransport{}
}
