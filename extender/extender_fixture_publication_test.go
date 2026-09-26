package extender

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Forward authorization reads the server's policy, while the reverse proxy
// snapshots it at construction. Both must see the complete operator policy;
// changing it after construction is observable even without a racing request.
func TestExtenderMobileMemoryFixturePublishesAuthorizationBeforeServing(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	fixture, _, _ := newExtenderMemoryFixture(t, ctx, connect.TransportModeH1)
	for _, host := range []string{"dest.example", "dest4.example", "dest6.example", "127.0.0.1", "alt.invalid"} {
		forward := fixture.server.IsAllowedHost(host)
		proxy := fixture.server.proxy.isWhitelisted(host)
		if !forward || !proxy {
			t.Errorf("configured host %q was not published to both authorization paths: forward=%t proxy=%t", host, forward, proxy)
		}
	}
	if fixture.server.IsAllowedHost("unlisted.invalid") || fixture.server.proxy.isWhitelisted("unlisted.invalid") {
		t.Error("publishing fixture hosts broadened authorization to an unlisted host")
	}
	// Spoof names are intentionally proxy-only, not forwarding destinations.
	if fixture.server.IsAllowedHost(testSpoofName) || !fixture.server.proxy.isWhitelisted(testSpoofName) {
		t.Error("operator policy publication changed the separate spoof-name policy")
	}
}
