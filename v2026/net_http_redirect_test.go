package connect

// Real local HTTP endpoints prove request-scoped redirect admission. Every
// handler consumes finite input, and each strategy joins before server cleanup.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestHttpRequestRedirectScopePreservesOtherOperations(t *testing.T) {
	for _, serial := range []bool{false, true} {
		for _, crossOrigin := range []bool{false, true} {
			for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
				func() {
					want := []byte("synthetic exact mutation")
					var initial, redirected atomic.Uint64
					target := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						raw, err := io.ReadAll(io.LimitReader(r.Body, 1024))
						closeErr := r.Body.Close()
						if err != nil || closeErr != nil || r.Method != http.MethodPost || !bytes.Equal(raw, want) {
							t.Error("redirect fixture changed request method/body")
						}
						redirected.Add(1)
						_, _ = w.Write([]byte("synthetic legacy allocation"))
					})
					targetServer := httptest.NewServer(target)
					defer targetServer.Close()
					destination := "/legacy"
					if crossOrigin {
						destination = targetServer.URL + "/legacy"
					}
					origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/legacy" {
							target.ServeHTTP(w, r)
							return
						}
						raw, err := io.ReadAll(io.LimitReader(r.Body, 1024))
						closeErr := r.Body.Close()
						if err != nil || closeErr != nil {
							t.Error("redirect fixture did not consume bounded input")
						}
						if r.URL.Path == "/hello" {
							w.WriteHeader(http.StatusOK)
							return
						}
						if r.Method != http.MethodPost || !bytes.Equal(raw, want) || r.Header.Get("Authorization") != "Bearer synthetic credential" {
							t.Error("redirect policy changed original request ownership")
						}
						initial.Add(1)
						w.Header().Set("Location", destination)
						w.WriteHeader(status)
					}))
					defer origin.Close()
					settings := DefaultClientStrategySettings()
					settings.EnableResilient = false
					settings.RequestTimeout, settings.ConnectTimeout = 5*time.Minute, time.Minute
					strategy := NewClientStrategy(t.Context(), settings)
					defer strategy.Close()
					for _, scoped := range []bool{true, false, true} {
						ctx := t.Context()
						if scoped {
							ctx = WithHttpRedirectsDisabled(ctx)
						}
						request, err := http.NewRequestWithContext(ctx, http.MethodPost, origin.URL+"/original", bytes.NewReader(want))
						if err != nil {
							t.Fatal(err)
						}
						request.Header.Set("Authorization", "Bearer synthetic credential")
						headers := request.Header.Clone()
						hello, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL+"/hello", nil)
						if err != nil {
							t.Fatal(err)
						}
						prior := redirected.Load()
						var result *httpResult
						if serial {
							result, err = strategy.HttpSerial(request, hello)
						} else {
							result, err = strategy.HttpParallel(request)
						}
						if err != nil || result == nil {
							t.Fatalf("completed redirect became a transport failure: serial=%v scoped=%v error=%v", serial, scoped, err)
						}
						if scoped && (result.response.StatusCode != status || redirected.Load() != prior) {
							t.Fatalf("scoped mutation followed redirect: serial=%v cross=%v status=%d targets=%d", serial, crossOrigin, status, redirected.Load()-prior)
						}
						if !scoped && (result.response.StatusCode != http.StatusOK || redirected.Load() != prior+1) {
							t.Fatal("scoped policy changed unrelated cached-client redirect behavior")
						}
						if !reflect.DeepEqual(request.Header, headers) || request.URL.String() != origin.URL+"/original" || request.Method != http.MethodPost {
							t.Fatal("redirect policy mutated caller request ownership")
						}
					}
					if initial.Load() != 3 {
						t.Fatalf("completed redirect retried original mutation: %d", initial.Load())
					}
				}()
			}
		}
	}
}

// A physical body interruption still retries the same endpoint and exact
// mutation bytes. Reconnect pacing is not the ordering/failure proof.
func TestHttpRequestRedirectScopeRetainsOriginalRetry(t *testing.T) {
	for _, serial := range []bool{false, true} {
		func() {
			want := []byte("synthetic retained operation")
			var attempts atomic.Uint64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(io.LimitReader(r.Body, 1024))
				closeErr := r.Body.Close()
				if err != nil || closeErr != nil {
					t.Error("retry fixture did not consume finite input")
					return
				}
				if r.URL.Path == "/hello" {
					return
				}
				if r.URL.Path != "/original" || !bytes.Equal(raw, want) || r.Header.Get("Authorization") != "Bearer synthetic credential" {
					t.Error("retry escaped original endpoint, bytes or credential")
				}
				if attempts.Add(1) == 1 {
					connection, buffered, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_, _ = fmt.Fprint(buffered, "HTTP/1.1 200 OK\r\nContent-Length: 128\r\nConnection: close\r\n\r\n{")
					_ = buffered.Flush()
					_ = connection.Close()
					return
				}
				_, _ = w.Write([]byte("synthetic complete reply"))
			}))
			defer server.Close()
			settings := DefaultClientStrategySettings()
			settings.EnableResilient = false
			settings.RequestTimeout, settings.ConnectTimeout = 5*time.Minute, time.Minute
			settings.ReconnectTimeout = time.Millisecond
			strategy := NewClientStrategy(t.Context(), settings)
			defer strategy.Close()
			ctx := WithHttpRedirectsDisabled(t.Context())
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/original", bytes.NewReader(want))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer synthetic credential")
			hello, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/hello", nil)
			if err != nil {
				t.Fatal(err)
			}
			var result *httpResult
			if serial {
				result, err = strategy.HttpSerial(request, hello)
			} else {
				result, err = strategy.HttpParallel(request)
			}
			if err != nil || result == nil || string(result.bodyBytes) != "synthetic complete reply" || attempts.Load() != 2 {
				t.Fatalf("scoped request lost genuine original-endpoint retry: serial=%v attempts=%d error=%v", serial, attempts.Load(), err)
			}
		}()
	}
}

// Client and browser admission copy only policy-bearing request state. Browser
// fetch options are checked here; actual browser execution is a separate scope.
func TestHttpRequestRedirectPolicyKeepsClientAndBrowserOwnership(t *testing.T) {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://synthetic.example/owned", nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: time.Minute}
	if httpClientForRequest(client, request) != client || httpBrowserRequestForRedirectPolicy(request) != request {
		t.Fatal("unscoped request acquired a new policy")
	}
	request = request.WithContext(WithHttpRedirectsDisabled(request.Context()))
	request.Header.Set("Authorization", "Bearer synthetic credential")
	owned := httpClientForRequest(client, request)
	if owned == client || owned.Timeout != client.Timeout || client.CheckRedirect != nil || owned.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("request policy mutated shared client state")
	}
	browser := httpBrowserRequestForRedirectPolicy(request)
	if browser == request || browser.Header.Get("js.fetch:redirect") != "error" || request.Header.Get("js.fetch:redirect") != "" || browser.Header.Get("Authorization") != request.Header.Get("Authorization") {
		t.Fatal("browser fetch could redirect or mutate caller headers")
	}
	browser.Header.Del("js.fetch:redirect")
	if request.Header.Get("Authorization") != "Bearer synthetic credential" {
		t.Fatal("browser consumed caller-owned header state")
	}
	for _, item := range []struct {
		name   string
		header http.Header
	}{
		{name: "nil"},
		{name: "empty", header: make(http.Header)},
	} {
		original := request.Clone(request.Context())
		original.Header = item.header
		browser, panicValue := func() (browser *http.Request, panicValue any) {
			defer func() { panicValue = recover() }()
			return httpBrowserRequestForRedirectPolicy(original), nil
		}()
		if panicValue != nil || browser == nil || browser.Header.Get("js.fetch:redirect") != "error" {
			t.Fatalf("browser scoped %s header could not retain redirect refusal: panic=%v", item.name, panicValue)
		}
		browser.Header.Set("Synthetic-Owned", "copied")
		if len(original.Header) != 0 || (original.Header == nil) != (item.header == nil) {
			t.Fatalf("browser scoped %s header mutated caller ownership", item.name)
		}
	}
}
