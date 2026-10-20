package extender

// Duration-only fixture traffic is bounded independently of loopback speed.
// Production caps, phase timers, admission and byte accounting stay untouched.

import (
	"context"
	"net/http"
	"testing"
)

// Two complete protocol frames leave ample room below the unchanged signed cap.
const testLagDurationByteCount = 2 * lagApiBufferByteCount

// Installs the test writer before the listener starts. Other endpoints, headers,
// real TLS framing and the production handler's stream admission are unchanged.
func newLagApiDurationFixture(t *testing.T, configure func(*ExtenderLagSettings)) *lagApiFixture {
	t.Helper()
	fixture := newLagApiFixture(t, configure)
	handler := fixture.api.httpServer.Handler
	fixture.api.httpServer.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/lag/v1/down" {
			if session, _, _ := fixture.api.streamSession(r); session != nil {
				phase := func() *lagApiPhase {
					session.stateLock.Lock()
					defer session.stateLock.Unlock()
					if session.load == nil {
						return nil
					}
					return session.load.download
				}
				w = newLagTestDurationWriter(w, r.Context(), phase)
			}
		}
		handler.ServeHTTP(w, r)
	})
	return fixture
}

// Only the response's owning writer calls Write; cancellation may be concurrent.
// No payload is retained. The blocked frame remains the production writer's
// reservation until Write returns zero and writeDownload refunds it once.
type lagTestDurationWriter struct {
	http.ResponseWriter
	ctx                context.Context
	phase              func() *lagApiPhase
	remainingByteCount int
}

// Returns a test-only bounded writer; it does not change production throughput.
func newLagTestDurationWriter(w http.ResponseWriter, ctx context.Context, phase func() *lagApiPhase) http.ResponseWriter {
	return &lagTestDurationWriter{
		ResponseWriter:     w,
		ctx:                ctx,
		phase:              phase,
		remainingByteCount: testLagDurationByteCount,
	}
}

// Preserves the real response controller's read and write deadline operations.
func (self *lagTestDurationWriter) Unwrap() http.ResponseWriter {
	return self.ResponseWriter
}

// Sends the finite frames, then waits for the exact phase or request to end.
// Returning no bytes on release leaves the real reservation refund in charge.
func (self *lagTestDurationWriter) Write(bytes []byte) (int, error) {
	if len(bytes) == 0 {
		return 0, nil
	}
	if 0 < self.remainingByteCount {
		count := min(len(bytes), self.remainingByteCount)
		n, err := self.ResponseWriter.Write(bytes[:count])
		self.remainingByteCount -= n
		return n, err
	}
	phase := self.phase()
	if phase == nil {
		return 0, context.Canceled
	}
	select {
	case <-self.ctx.Done():
		return 0, self.ctx.Err()
	case <-phase.ctx.Done():
		return 0, phase.ctx.Err()
	}
}
