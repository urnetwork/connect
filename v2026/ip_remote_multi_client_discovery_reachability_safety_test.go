package connect

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type discoveryReachabilityHttpEvent struct {
	method byte
	socket uint64
}

type discoveryReachabilityHttpFixture struct {
	server     *httptest.Server
	strategy   *ClientStrategy
	credential string
	provider   Id
	lock       sync.Mutex
	events     []discoveryReachabilityHttpEvent
	postCount  int
}

// Each POST is counted only after the handler validates its bearer and decodes
// the complete original query. A test response can act on a chosen POST.
func newDiscoveryReachabilityHttpFixture(t *testing.T, strategy *ClientStrategy, respond func(http.ResponseWriter, *http.Request, int) bool) *discoveryReachabilityHttpFixture {
	t.Helper()
	f := &discoveryReachabilityHttpFixture{strategy: strategy, credential: rand.Text(), provider: NewId()}
	type socketKey struct{}
	var nextSocket atomic.Uint64
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, _ := r.Context().Value(socketKey{}).(uint64)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/hello":
			f.lock.Lock()
			f.events = append(f.events, discoveryReachabilityHttpEvent{'H', socket})
			f.lock.Unlock()
			_, _ = io.WriteString(w, "{\"client_address\":\"127.0.0.1\"}")
		case r.Method == http.MethodPost && r.URL.Path == "/network/find-providers2":
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+f.credential)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			var args FindProviders2Args
			if err := json.NewDecoder(r.Body).Decode(&args); err != nil || args.RankMode != "quality" ||
				args.Count != 1 || len(args.Specs) != 1 || args.Specs[0] == nil ||
				args.Specs[0].ClientId == nil || *args.Specs[0].ClientId != f.provider {
				http.Error(w, "invalid discovery", http.StatusBadRequest)
				return
			}
			f.lock.Lock()
			f.events = append(f.events, discoveryReachabilityHttpEvent{'P', socket})
			f.postCount++
			ordinal := f.postCount
			f.lock.Unlock()
			if respond != nil && respond(w, r, ordinal) {
				return
			}
			_ = json.NewEncoder(w).Encode(FindProviders2Result{Providers: []*FindProvidersProvider{{ClientId: f.provider}}})
		default:
			http.NotFound(w, r)
		}
	}))
	f.server.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, socketKey{}, nextSocket.Add(1))
	}
	f.server.Start()
	t.Cleanup(f.server.Close)
	return f
}

func (f *discoveryReachabilityHttpFixture) lookup(ctx context.Context) error {
	api := NewBringYourApi(ctx, f.strategy, f.server.URL)
	defer api.Close()
	api.SetByJwt(f.credential)
	result, err := api.FindProviders2SyncWithCtx(ctx, &FindProviders2Args{
		Specs: []*ProviderSpec{{ClientId: &f.provider}}, Count: 1, RankMode: "quality",
	})
	if err == nil && (result == nil || len(result.Providers) != 1 || result.Providers[0].ClientId != f.provider) {
		return fmt.Errorf("missing provider from decoded authenticated lookup")
	}
	return err
}

func (f *discoveryReachabilityHttpFixture) mustLookup(t *testing.T) {
	t.Helper()
	if err := f.lookup(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func (f *discoveryReachabilityHttpFixture) requireEvents(t *testing.T, methods string, sockets ...uint64) {
	t.Helper()
	f.lock.Lock()
	events := slices.Clone(f.events)
	f.lock.Unlock()
	var got strings.Builder
	var ids []uint64
	for _, event := range events {
		got.WriteByte(event.method)
		ids = append(ids, event.socket)
	}
	if got.String() != methods || !slices.Equal(ids, sockets) {
		t.Fatalf("observed methods=%s sockets=%v, want %s %v", got.String(), ids, methods, sockets)
	}
}

func TestRepeatFindProvidersTruncatedResponseDoesNotReplayWrittenPost(t *testing.T) {
	strategy, _ := newDeliveryTestStrategy(t, nil)
	f := newDiscoveryReachabilityHttpFixture(t, strategy, func(w http.ResponseWriter, _ *http.Request, ordinal int) bool {
		if ordinal != 2 {
			return false
		}
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack actual HTTP/1 connection: %v", err)
			return true
		}
		defer conn.Close()
		if _, err := io.WriteString(buffered, "HTTP/1.1 200 OK\r\nContent-Length: 1024\r\nConnection: close\r\n\r\n{\"providers\":["); err != nil {
			t.Errorf("write partial response: %v", err)
		} else if err := buffered.Flush(); err != nil {
			t.Errorf("flush partial response: %v", err)
		}
		return true
	})
	f.mustLookup(t)
	f.requireEvents(t, "HP", 1, 1)
	if err := f.lookup(t.Context()); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected truncated response to the one written POST, got %v", err)
	}
	f.requireEvents(t, "HPP", 1, 1, 1)
	f.mustLookup(t)
	f.requireEvents(t, "HPPHP", 1, 1, 1, 2, 2)
	t.Log("DISCOVERY_WRITTEN_POST_TRUNCATION_JOINED one_authorized_hot_post=1 duplicate_posts=0 fresh_hello=1")
}

func TestRepeatFindProvidersOriginReachabilityResetsWithPoolAndNetwork(t *testing.T) {
	for _, scenario := range []string{"pool", "network-roundtrip"} {
		t.Run(scenario, func(t *testing.T) {
			strategy, _ := newDeliveryTestStrategy(t, nil)
			strategy.SetNetworkId("discovery-network-a")
			f := newDiscoveryReachabilityHttpFixture(t, strategy, nil)
			f.mustLookup(t)
			f.mustLookup(t)
			f.requireEvents(t, "HPP", 1, 1, 1)
			var next uint64 = 1
			if scenario == "pool" {
				strategy.CloseIdleConnections()
				next = 2
			} else {
				strategy.SetNetworkId("discovery-network-b")
				strategy.SetNetworkId("discovery-network-a")
			}
			f.mustLookup(t)
			f.mustLookup(t)
			f.requireEvents(t, "HPPHPP", 1, 1, 1, next, next, next)
			t.Logf("DISCOVERY_REACHABILITY_SCOPE_REQUALIFIED scope=%s original_posts=4 total_hellos=2", scenario)
		})
	}
}

func TestRepeatFindProvidersLatePositiveCannotReinstatePreviousNetwork(t *testing.T) {
	strategy, _ := newDeliveryTestStrategy(t, nil)
	strategy.SetNetworkId("discovery-network-a")
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	f := newDiscoveryReachabilityHttpFixture(t, strategy, func(_ http.ResponseWriter, r *http.Request, ordinal int) bool {
		if ordinal == 1 {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return true
			}
		}
		return false
	})
	done := make(chan error, 1)
	go func() { done <- f.lookup(t.Context()) }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("lookup ended before its decoded POST was held: %v", err)
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	strategy.SetNetworkId("discovery-network-b")
	strategy.SetNetworkId("discovery-network-a")
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.mustLookup(t)
	f.mustLookup(t)
	f.requireEvents(t, "HPHPP", 1, 1, 1, 1, 1)
	t.Log("DISCOVERY_LATE_PREVIOUS_NETWORK_RESPONSE_IGNORED same_socket=1 path_callbacks=2")
}

func TestRepeatFindProvidersOriginReachabilityCannotCrossAuthorities(t *testing.T) {
	strategy, _ := newDeliveryTestStrategy(t, nil)
	first := newDiscoveryReachabilityHttpFixture(t, strategy, nil)
	second := newDiscoveryReachabilityHttpFixture(t, strategy, nil)
	first.mustLookup(t)
	first.mustLookup(t)
	first.requireEvents(t, "HPP", 1, 1, 1)
	second.mustLookup(t)
	second.requireEvents(t, "HP", 1, 1)
	t.Log("DISCOVERY_DISTINCT_AUTHORITY_REQUALIFIED same_strategy=1 distinct_authorities=2")
}
