package connect

// This bridge exists only in connect's test variant. The external conformance
// package consumes the same five private fixture dialers and request wrappers;
// it does not add a production API or replace any dial path or parser oracle.

import (
	"net/http"
	"slices"
	"testing"
)

// An external view of one existing private fixture row, with its full dialer
// flags retained by value for the request path below.
type TlsHelloConformanceDialerForTest struct {
	Description string
	Fragment    bool
	dialer      testTlsHelloDialer
}

// Preserve every existing row and its order, including fragment+segment.
func TlsHelloConformanceDialersForTest() []TlsHelloConformanceDialerForTest {
	dialers := make([]TlsHelloConformanceDialerForTest, 0, len(testTlsHelloDialers))
	for _, dialer := range testTlsHelloDialers {
		dialers = append(dialers, TlsHelloConformanceDialerForTest{
			Description: dialer.description,
			Fragment:    dialer.fragment,
			dialer:      dialer,
		})
	}
	return dialers
}

// Preserve the existing api-then-websocket order and both helpers'
// real transport, response, cleanup and timeout checks.
func (self TlsHelloConformanceDialerForTest) Capture(t *testing.T, settings *ClientStrategySettings, authority string) {
	t.Helper()
	dialer := self.dialer.clientDialer(settings)
	client := dialer.HttpClient()
	testTlsHelloApiRequest(t, client, authority)
	client.CloseIdleConnections()
	testTlsHelloWebSocket(t, dialer, authority)
}

// Use the same request handler as the private dialer fixture.
func TlsHelloConformanceHandlerForTest() http.Handler {
	return testTlsHelloHandler()
}

// Read the real path protocols without giving external tests mutable aliases.
func TlsHelloConformanceProtocolsForTest() (httpProtocols, webSocketProtocols []string) {
	return slices.Clone(clientHttpNextProtos), slices.Clone(clientWebSocketNextProtos)
}

const TlsHelloConformanceHttpStateForTest = httpTransportReadsTlsConnectionState
