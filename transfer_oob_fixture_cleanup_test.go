// Fixture cleanup joins blocked HTTP handlers only after releasing their owned
// gates, including the runtime.Goexit unwind used by testing.T.FailNow.
package connect

import (
	"context"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"
)

// Callback release and cancellation are registered later by the fixture and
// unwind first. The handler gate must open before the HTTP server is joined.
func closeOobHandlerFixture(releaseHandler func(), closeServer func()) {
	releaseHandler()
	closeServer()
}

// The real fixture and this regression use the same cleanup owner. An explicit
// join-entry observation catches reversed ordering without waiting for a timeout.
func TestOobHandlerFixtureFailureUnwindReleasesBeforeJoin(t *testing.T) {
	for _, ipVersion := range testIpVersions {
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			handlerEntered := make(chan struct{})
			releaseHandler := make(chan struct{})
			handlerDone := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseHandler) }) }
			server := newFamilyHttptestServer(t, ipVersion, http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				close(handlerEntered)
				defer close(handlerDone)
				<-releaseHandler
				response.WriteHeader(http.StatusNoContent)
			}))
			requestCtx, cancelRequest := context.WithCancel(ctx)
			requestDone := make(chan struct{})
			ownerDone := make(chan struct{})
			ownerStarted := false
			defer func() {
				// Rescue the old ordering even on assertion failure, then join
				// every admitted goroutine before this family iteration ends.
				release()
				cancelRequest()
				server.Close()
				<-requestDone
				if ownerStarted {
					<-ownerDone
				}
			}()
			request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, server.URL, nil)
			if err != nil {
				close(requestDone)
				t.Fatal(err)
			}
			go func() {
				defer close(requestDone)
				response, err := server.Client().Do(request)
				if err == nil {
					response.Body.Close()
				}
			}()
			select {
			case <-handlerEntered:
			case <-ctx.Done():
				t.Fatalf("v%d handler did not reach its owned gate", ipVersion)
			}

			callbackReleased := make(chan struct{})
			closeEntered := make(chan bool, 1)
			closeServer := func() {
				released := false
				select {
				case <-releaseHandler:
					released = true
				default:
				}
				closeEntered <- released
				server.Close()
			}
			ownerStarted = true
			go func() {
				defer close(ownerDone)
				defer closeOobHandlerFixture(release, closeServer)
				defer cancelRequest()
				defer close(callbackReleased)
				// Exercise FailNow's actual unwind primitive without causing
				// an unrelated testing.T failure or spawning a subprocess.
				runtime.Goexit()
			}()

			var releasedBeforeJoin bool
			select {
			case releasedBeforeJoin = <-closeEntered:
			case <-ctx.Done():
				t.Fatalf("v%d failure unwind did not reach server close", ipVersion)
			}
			select {
			case <-callbackReleased:
			default:
				t.Errorf("v%d callback release did not precede server close", ipVersion)
			}
			if requestCtx.Err() == nil {
				t.Errorf("v%d request cancellation did not precede server close", ipVersion)
			}
			if !releasedBeforeJoin {
				// The active handler needs release(), so the cleanup owner
				// cannot finish a real Server.Close at this exact boundary.
				select {
				case <-ownerDone:
					t.Errorf("v%d owner returned through an unreleased handler", ipVersion)
				default:
				}
				t.Errorf("v%d server join reached while its handler gate was retained", ipVersion)
			}
			release()
			select {
			case <-ownerDone:
			case <-ctx.Done():
				t.Fatalf("v%d owner did not join after handler release", ipVersion)
			}
			select {
			case <-handlerDone:
			case <-ctx.Done():
				t.Fatalf("v%d server returned without joining its handler", ipVersion)
			}
			select {
			case <-requestDone:
			case <-ctx.Done():
				t.Fatalf("v%d client request did not join", ipVersion)
			}
		}()
	}
}
