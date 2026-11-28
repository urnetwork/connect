package connect

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// After one successful authenticated read-only lookup, its sole direct route
// can carry the next lookup. A separate GET on the same live HTTP/1 socket
// introduces a dependency without discovering an alternative route.
// The plaintext literal TUN route reuses evidence from its verified socket.
func TestRepeatFindProvidersOnSoleDirectRouteUsesVerifiedOrigin(t *testing.T) {
	testRepeatVerifiedDiscovery(t, false)
}

// The TLS HTTP/1 route has the same evidence requirement.
func TestRepeatFindProvidersOnSoleDirectTlsRouteUsesVerifiedOrigin(t *testing.T) {
	testRepeatVerifiedDiscovery(t, true)
}

func testRepeatVerifiedDiscovery(t *testing.T, secure bool) {
	credential := rand.Text()
	provider := NewId()
	type socketKey struct{}
	var nextSocket, hellos, posts atomic.Uint64
	var wrongSocket atomic.Bool
	repeatHello := make(chan struct{})
	release := make(chan struct{})
	openGate := sync.OnceFunc(func() { close(release) })
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, _ := r.Context().Value(socketKey{}).(uint64)
		if socket != 1 || (r.TLS != nil) != secure || r.ProtoMajor != 1 {
			wrongSocket.Store(true)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/hello":
			if hellos.Add(1) == 2 {
				close(repeatHello)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			_, _ = w.Write([]byte("{\"client_address\":\"127.0.0.1\"}"))
		case r.Method == http.MethodPost && r.URL.Path == "/network/find-providers2":
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+credential)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			var query FindProviders2Args
			if err := json.NewDecoder(r.Body).Decode(&query); err != nil || query.RankMode != "quality" ||
				query.Count != 1 || len(query.Specs) != 1 || query.Specs[0] == nil || query.Specs[0].ClientId == nil || *query.Specs[0].ClientId != provider {
				http.Error(w, "invalid discovery", http.StatusBadRequest)
				return
			}
			posts.Add(1)
			_ = json.NewEncoder(w).Encode(FindProviders2Result{Providers: []*FindProvidersProvider{{ClientId: provider}}})
		default:
			http.NotFound(w, r)
		}
	}))
	server.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, socketKey{}, nextSocket.Add(1))
	}
	settings := DefaultClientStrategySettings()
	settings.EnableResilient = false
	settings.TlsClientHelloFingerprint = ""
	var tunnel *dohProgressTunFixture
	if secure {
		server.StartTLS()
		settings.TlsConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	} else {
		tunnel = newDohProgressTunFixture(t, 4)
		listener, err := tunnel.right.ListenTCP(&net.TCPAddr{IP: net.IP(tunTestLocalAddress(t, tunnel.right, 4).AsSlice())})
		if err != nil {
			t.Fatal(err)
		}
		if err := server.Listener.Close(); err != nil {
			t.Fatal(err)
		}
		server.Listener = listener
		server.Start()
		settings.ConnectSettings.DialContextSettings = &DialContextSettings{DialContext: tunnel.left.DialContext}
	}
	defer server.Close()
	defer openGate()
	strategy := NewClientStrategy(t.Context(), settings)
	defer strategy.Close()
	if len(strategy.dialers) != 1 {
		t.Fatalf("expected one direct route, got %d", len(strategy.dialers))
	}
	var direct *clientDialer
	for candidate := range strategy.dialers {
		direct = candidate
	}
	if direct.description != "normal" {
		t.Fatal("the sole route is not the configured normal direct dialer")
	}
	lookup := func(ctx context.Context) error {
		// The production fixture rebuilds its API wrapper for every lookup. The
		// route owner and pool are shared across wrappers.
		api := NewBringYourApi(ctx, strategy, server.URL)
		defer api.Close()
		api.SetByJwt(credential)
		result, err := api.FindProviders2SyncWithCtx(ctx, &FindProviders2Args{
			Specs: []*ProviderSpec{{ClientId: &provider}}, RankMode: "quality", Count: 1,
		})
		if err == nil && (result == nil || len(result.Providers) != 1 || result.Providers[0].ClientId != provider) {
			return fmt.Errorf("missing provider from original authenticated lookup")
		}
		return err
	}
	if err := lookup(t.Context()); err != nil {
		t.Fatal(err)
	}
	neutral := func() bool {
		return strategy.scores.weight("", direct.dialerKey()) == 1 && !strategy.dialerDelivered(direct) &&
			(tunnel == nil || (tunnel.readyQueries.Load() == 0 && tunnel.heldQueries.Load() == 0))
	}
	if hellos.Load() != 1 || posts.Load() != 1 || wrongSocket.Load() || nextSocket.Load() != 1 || !neutral() {
		t.Fatalf("initial discovery failed its ownership premises: hello=%d post=%d accepted_sockets=%d neutral=%v",
			hellos.Load(), posts.Load(), nextSocket.Load(), neutral())
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- lookup(ctx) }()
	select {
	case <-repeatHello:
		cancel()
		openGate()
		err := <-done
		if err == nil || hellos.Load() != 2 || posts.Load() != 1 || wrongSocket.Load() || nextSocket.Load() != 1 || !neutral() {
			t.Fatalf("the held repeat did not retire its exact operation: err=%v hello=%d post=%d sockets=%d neutral=%v",
				err, hellos.Load(), posts.Load(), nextSocket.Load(), neutral())
		}
		t.Fatal("REPEAT_DISCOVERY_HELLO_BLOCKS_AUTHENTICATED_POST authorized_first_post=1 repeated_hello=1 second_post=0 same_socket=1 score_neutral=1")
	case err := <-done:
		if err != nil || hellos.Load() != 1 || posts.Load() != 2 || wrongSocket.Load() || nextSocket.Load() != 1 || !neutral() {
			t.Fatalf("repeat lookup failed its original authority or route constraints: err=%v hello=%d post=%d sockets=%d neutral=%v",
				err, hellos.Load(), posts.Load(), nextSocket.Load(), neutral())
		}
		t.Log("REPEAT_DISCOVERY_AUTHENTICATED_POST_ON_VERIFIED_ORIGIN authorized_posts=2 total_hello=1 same_socket=1 score_neutral=1")
	case <-ctx.Done():
		openGate()
		<-done
		t.Fatal("repeat discovery never reached an attributable HTTP handler")
	}
}
