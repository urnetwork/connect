// A client's dns carrier dial races the extender's dns ports (EXTENDER.md L2,
// connect/net_extender_dns_ports.go). Over real sockets, an extender that
// listens on one dns port is reached through the carrier whether the port
// that never answers -- 53 on an app extender, which binds 4053 alone, or on a
// network that blackholes 53 -- is raced before it or after it.

package extender

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

func TestDnsCarrierDialReachesTheExtenderPastABlackholedPort(t *testing.T) {
	fixture := newExtenderFixture(t, "127.0.0.1", nil)
	// a bound socket nothing reads answers nothing and sends no unreachable,
	// which is what a port the extender does not bind looks like from a
	// network that drops it
	blackhole, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blackhole.Close() })
	blackholePort := blackhole.LocalAddr().(*net.UDPAddr).Port

	cases := []struct {
		name     string
		dnsPorts []int
	}{
		// a record that lists 53, dialed 53 first, where 53 never answers
		{name: "blackholed port first", dnsPorts: []int{blackholePort, fixture.dnsPort}},
		// an app extender's record, dialed 4053 first and 53 behind it
		{name: "blackholed port second", dnsPorts: []int{fixture.dnsPort, blackholePort}},
	}
	for _, c := range cases {
		extenderConfig := fixture.extenderConfig(connect.ExtenderCarrierDns)
		extenderConfig.Profile.Port = c.dnsPorts[0]
		extenderConfig.DnsPorts = c.dnsPorts
		client := connect.NewExtenderHttpClient(fixture.connectSettings(), extenderConfig)

		response, err := client.Get("https://dest.example/hello")
		if err != nil {
			if extenderErr, ok := fixture.nextError(); ok {
				t.Fatalf("%s: %v; extender: %v", c.name, err, extenderErr)
			}
			t.Fatalf("%s: %v", c.name, err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, expected %d", c.name, response.StatusCode, http.StatusOK)
		}
		if !strings.Contains(string(body), "dest.example") {
			t.Fatalf("%s: body = %q", c.name, body)
		}
		client.CloseIdleConnections()
	}
}
