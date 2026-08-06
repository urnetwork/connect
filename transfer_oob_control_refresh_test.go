package connect

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func TestApiOutOfBandControlUsesRefreshedJwt(t *testing.T) {
	authorizations := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations <- r.Header.Get("Authorization")
		var args ConnectControlArgs
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&ConnectControlResult{Pack: args.Pack})
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	control := NewApiOutOfBandControl(ctx, NewClientStrategyWithDefaults(ctx), "old-jwt", server.URL)

	send := func() {
		done := make(chan error, 1)
		control.SendControl([]*protocol.Frame{}, func(_ []*protocol.Frame, err error) {
			done <- err
		})
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("out-of-band control request timed out")
		}
	}

	send()
	control.SetByJwt("new-jwt")
	send()

	for i, want := range []string{"Bearer old-jwt", "Bearer new-jwt"} {
		select {
		case got := <-authorizations:
			if got != want {
				t.Fatalf("request %d authorization = %q, want %q", i+1, got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("missing authorization for request %d", i+1)
		}
	}
}
