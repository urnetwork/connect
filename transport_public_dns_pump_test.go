// No connect test may contact the public DNS pump host, the operator's live
// host that the production default names for H3DnsPump. An Auto transport
// whose H1 is down reaches the pump tier on its own, so any test that keeps
// the defaults would resolve and dial it. The package's tests switch it off
// once, here, for every test in this binary. Test binaries of the other
// packages (extender, connectctl and the rest) do not run this init, so a test
// there that builds an Auto or pump transport names its own pump host, as the
// extender pump tests do.
package connect

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
)

func init() {
	dialPublicDnsPumpHost = false
}

// Computes one H3DnsPump destination through a synthetic resolver that refuses
// every lookup, so nothing is sent. Returns how many lookups the resolver saw
// and the dial error.
func lookUpDnsPumpCandidates(t *testing.T, settings *PlatformTransportSettings) (int64, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var lookupCount atomic.Int64
	strategySettings := DefaultClientStrategySettings()
	strategySettings.ConnectSettings.Resolver = &net.Resolver{
		PreferGo: true,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			lookupCount.Add(1)
			return nil, errors.New("synthetic resolver refuses every lookup")
		},
	}
	strategy := NewClientStrategy(ctx, strategySettings)
	defer strategy.Close()
	transport := &PlatformTransport{settings: settings, clientStrategy: strategy}
	candidateAddrs, _, source, err := transport.h3DialCandidatesProgressive(ctx, TransportModeH3DnsPump, "platform.example")
	source.close()
	if err == nil {
		t.Fatalf("pump destination resolved to %v through a resolver that refuses every lookup", candidateAddrs)
	}
	return lookupCount.Load(), err
}

// A transport left on the default settings asks no resolver for its pump
// host, so its pump tier has nothing to dial. A test that names its own
// synthetic pump host still reaches the resolver, so the refusal covers the
// production default and nothing else.
func TestPackageTestsNeverResolveOrDialPublicDnsPumpHost(t *testing.T) {
	defaultLookupCount, defaultErr := lookUpDnsPumpCandidates(t, DefaultPlatformTransportSettings())
	if defaultLookupCount != 0 || !strings.Contains(defaultErr.Error(), "public H3 DNS pump host") {
		t.Fatalf("a transport on default settings looked up its pump host %d time(s), error %v; want the refusal before any lookup", defaultLookupCount, defaultErr)
	}

	settings := DefaultPlatformTransportSettings()
	settings.DnsPumpHost = "pump.example"
	syntheticLookupCount, syntheticErr := lookUpDnsPumpCandidates(t, settings)
	if syntheticLookupCount == 0 || strings.Contains(syntheticErr.Error(), "public H3 DNS pump host") {
		t.Fatalf("a synthetic pump host was looked up %d time(s), error %v; want an ordinary lookup", syntheticLookupCount, syntheticErr)
	}
}
