package connect

import (
	"context"
	"testing"
)

// A platform transport pings over the extender carrier's one request stream,
// and that ping is stream data, which keeps the outer QUIC connection active.
// So a carrier the platform transport owns sends no QUIC PING of its own:
// before, it added one every PingTimeout on top of the transport's ping. A
// feed, API or probe dial owns its carrier alone and keeps the keepalive, and
// the idle timeout is the same either way.
func TestExtenderQuicCarrierKeepAliveOnlyWithoutAPlatformTransport(t *testing.T) {
	settings := DefaultPlatformTransportSettings()
	connectSettings := DefaultConnectSettings()
	owned := newExtenderQuicMemoryPolicy(
		(&PlatformTransport{settings: settings}).dialContext(context.Background()),
		connectSettings,
	)
	unowned := newExtenderQuicMemoryPolicy(context.Background(), connectSettings)

	if owned.quicConfig.KeepAlivePeriod != 0 {
		t.Errorf("a platform transport's carrier keeps a %s QUIC keepalive", owned.quicConfig.KeepAlivePeriod)
	}
	if unowned.quicConfig.KeepAlivePeriod != DefaultPlatformTransportSettings().PingTimeout {
		t.Errorf("a standalone carrier's QUIC keepalive is %s, expected the ping timeout %s", unowned.quicConfig.KeepAlivePeriod, DefaultPlatformTransportSettings().PingTimeout)
	}
	if owned.quicConfig.MaxIdleTimeout != unowned.quicConfig.MaxIdleTimeout || owned.quicConfig.MaxIdleTimeout <= 2*settings.PingTimeout {
		t.Errorf("idle timeouts owned %s and standalone %s, expected the same and more than two pings apart", owned.quicConfig.MaxIdleTimeout, unowned.quicConfig.MaxIdleTimeout)
	}
}
