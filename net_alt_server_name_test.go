package connect

import (
	"testing"
)

// The alt host and port of each server name, the rule the alt dialers dial by
// (net_http_alt_family_test.go; EXTENDER.md L3, L4; connect#229): a family
// name of the alt url's own space takes the family's alt name and the alt
// url's port, and every other name, or an alt url set explicitly, keeps the
// alt url's own host.
func TestAltHostPortForServerName(t *testing.T) {
	cases := []struct {
		altUrl     string
		serverName string
		host       string
		port       int
	}{
		{altUrl: "wss://alt.space.example", serverName: "api-v4.space.example", host: "alt-v4.space.example"},
		{altUrl: "wss://alt.space.example", serverName: "api-v6.space.example", host: "alt-v6.space.example"},
		{altUrl: "wss://alt.space.example", serverName: "connect-v4.space.example", host: "alt-v4.space.example"},
		{altUrl: "wss://alt.space.example", serverName: "connect-v6.space.example", host: "alt-v6.space.example"},
		// the env prefix of the alt url's space, and a name in mixed case with
		// a trailing dot
		{altUrl: "https://g2-alt.space.example", serverName: "G2-Api-V4.Space.Example.", host: "g2-alt-v4.space.example"},
		// the alt url's port carries over, whatever port the request names
		{altUrl: "https://alt.space.example:8443", serverName: "api-v6.space.example:443", host: "alt-v6.space.example", port: 8443},
		// a family-agnostic name is served by the alt url's own host
		{altUrl: "wss://alt.space.example", serverName: "api.space.example", host: "alt.space.example"},
		{altUrl: "wss://alt.space.example", serverName: "connect.space.example", host: "alt.space.example"},
		// no service label, another env, another space, no name
		{altUrl: "wss://alt.space.example", serverName: "auth.space.example", host: "alt.space.example"},
		{altUrl: "wss://alt.space.example", serverName: "extender-v4.space.example", host: "alt.space.example"},
		{altUrl: "wss://alt.space.example", serverName: "g2-api-v4.space.example", host: "alt.space.example"},
		{altUrl: "wss://alt.space.example", serverName: "api-v4.other.example", host: "alt.space.example"},
		{altUrl: "wss://alt.space.example", serverName: "192.0.2.4", host: "alt.space.example"},
		{altUrl: "wss://alt.space.example", serverName: "", host: "alt.space.example"},
		// an alt url set explicitly is kept for every name
		{altUrl: "https://192.0.2.10:14443", serverName: "api-v4.space.example", host: "192.0.2.10", port: 14443},
		{altUrl: "https://alt-v4.space.example", serverName: "api-v6.space.example", host: "alt-v4.space.example"},
		// no alt url, no alt host
		{altUrl: "", serverName: "api-v4.space.example", host: ""},
	}
	for _, c := range cases {
		host, port := altHostPortForServerName(c.altUrl, c.serverName)
		if host != c.host || port != c.port {
			t.Errorf(
				"altHostPortForServerName(%q, %q) = %q/%d, expected %q/%d",
				c.altUrl, c.serverName, host, port, c.host, c.port)
		}
	}
}
