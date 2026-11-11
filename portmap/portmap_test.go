package portmap

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"
)

// Every map request a router saw names one of the requests and this host's own
// address: the mapper maps nothing else.
func testAssertOwnPortsOnly(t *testing.T, mapRequests []testMapRequest, requests []PortMapRequest) {
	t.Helper()
	for _, mapRequest := range mapRequests {
		if !slices.Contains(requests, PortMapRequest{Protocol: mapRequest.protocol, InternalPort: mapRequest.internalPort}) {
			t.Fatalf("the router was asked to map %s %d, which is not a request", mapRequest.protocol, mapRequest.internalPort)
		}
		if mapRequest.version == pcpVersion && mapRequest.clientIp != testLocalIp {
			t.Fatalf("a pcp request named client %s, expected this host %s", mapRequest.clientIp, testLocalIp)
		}
	}
}

func TestPortMapperMapsByPcp(t *testing.T) {
	clock := newTestClock()
	gateway := newTestPmpGateway()
	requests := testExtenderRequests()
	mapper := NewPortMapper(context.Background(), requests, testPortMapperSettings(clock, gateway, nil))
	defer mapper.Close()

	status := testWaitReady(t, mapper)
	if status.State != StateMapped || status.Method != MethodPcp {
		t.Fatalf("status = %+v, expected mapped by pcp", status)
	}
	if status.Gateway != testGatewayIp || status.LocalIp != testLocalIp || status.ExternalIp != testExternalIp {
		t.Fatalf("status addresses = %s / %s / %s", status.Gateway, status.LocalIp, status.ExternalIp)
	}
	if len(status.Mappings) != len(requests) || status.Error != "" {
		t.Fatalf("status = %+v, expected every request mapped", status)
	}
	for _, request := range requests {
		mapping := status.Mapping(request.Protocol, request.InternalPort)
		if mapping == nil || mapping.ExternalPort != request.InternalPort || mapping.Lifetime != time.Hour {
			t.Fatalf("%s mapping = %+v, expected the same external port for an hour", request, mapping)
		}
	}
	mapRequests := gateway.mapRequests()
	testAssertOwnPortsOnly(t, mapRequests, requests)
	nonces := map[[12]byte]bool{}
	for _, mapRequest := range mapRequests {
		nonces[mapRequest.nonce] = true
	}
	if len(nonces) != len(requests) {
		t.Fatalf("nonces = %d, expected one per mapping", len(nonces))
	}
}

func TestPortMapperFallsBackToNatPmp(t *testing.T) {
	clock := newTestClock()
	gateway := newTestPmpGateway()
	gateway.pcp = false
	mapper := NewPortMapper(context.Background(), testExtenderRequests(), testPortMapperSettings(clock, gateway, nil))
	defer mapper.Close()

	status := testWaitReady(t, mapper)
	if status.State != StateMapped || status.Method != MethodNatPmp {
		t.Fatalf("status = %+v, expected mapped by nat-pmp", status)
	}
	// the map answers of nat-pmp name no address; its own request does
	if status.ExternalIp != testExternalIp {
		t.Fatalf("external ip = %s, expected %s", status.ExternalIp, testExternalIp)
	}
	if held := gateway.held(); len(held) != 3 {
		t.Fatalf("the router holds %v, expected the three requests", held)
	}
	testAssertOwnPortsOnly(t, gateway.mapRequests(), testExtenderRequests())
}

func TestPortMapperOffersTheRoutersExternalPort(t *testing.T) {
	clock := newTestClock()
	gateway := newTestPmpGateway()
	gateway.externalPort = func(protocol string, internalPort int, suggestedExternal int) int {
		if protocol == ProtocolTcp {
			return 40443
		}
		return internalPort
	}
	mapper := NewPortMapper(context.Background(), testExtenderRequests(), testPortMapperSettings(clock, gateway, nil))
	defer mapper.Close()

	status := testWaitReady(t, mapper)
	if mapping := status.Mapping(ProtocolTcp, 443); mapping == nil || mapping.ExternalPort != 40443 {
		t.Fatalf("tcp mapping = %+v, expected the router's external port 40443", mapping)
	}
	if mapping := status.Mapping(ProtocolUdp, 443); mapping == nil || mapping.ExternalPort != 443 {
		t.Fatalf("udp mapping = %+v, expected 443", mapping)
	}
}

func TestPortMapperRenewsAtAFractionOfTheGrantedLifetime(t *testing.T) {
	clock := newTestClock()
	gateway := newTestPmpGateway()
	// the router grants ten minutes of the hour asked for
	gateway.lifetimeCapSeconds = 600
	mapper := NewPortMapper(context.Background(), testExtenderRequests(), testPortMapperSettings(clock, gateway, nil))
	defer mapper.Close()

	testWaitReady(t, mapper)
	firstMaps := gateway.mapRequests()
	wait := clock.nextWait(t)
	if wait.wait != 5*time.Minute {
		t.Fatalf("renewal wait = %s, expected half of the ten minutes granted", wait.wait)
	}

	_, update := mapper.ChangeMonitor().Get()
	clock.elapse(wait)
	select {
	case <-update:
	case <-time.After(30 * time.Second):
		t.Fatal("the renewal published nothing")
	}
	renewalMaps := gateway.mapRequests()[len(firstMaps):]
	if len(renewalMaps) != len(firstMaps) {
		t.Fatalf("renewal maps = %d, expected one per mapping", len(renewalMaps))
	}
	for i, renewal := range renewalMaps {
		// the same mapping, named by its nonce, on the port it had
		if renewal.nonce != firstMaps[i].nonce || renewal.suggestedExternal != firstMaps[i].internalPort {
			t.Fatalf("renewal %d = %+v, expected the nonce and external port of %+v", i, renewal, firstMaps[i])
		}
	}
	if next := clock.nextWait(t); next.wait != 5*time.Minute {
		t.Fatalf("next renewal wait = %s, expected 5m", next.wait)
	}
	testAssertOwnPortsOnly(t, gateway.mapRequests(), testExtenderRequests())
}

func TestPortMapperRenewalFollowsAMovedPort(t *testing.T) {
	clock := newTestClock()
	gateway := newTestPmpGateway()
	mapper := NewPortMapper(context.Background(), testExtenderRequests(), testPortMapperSettings(clock, gateway, nil))
	defer mapper.Close()

	testWaitReady(t, mapper)
	// the router rebooted and another host has 443 now
	gateway.set(func() {
		gateway.externalPort = func(protocol string, internalPort int, suggestedExternal int) int {
			if protocol == ProtocolTcp {
				return 41443
			}
			return internalPort
		}
	})
	clock.elapse(clock.nextWait(t))
	status := testWaitStatus(t, mapper, func(status *PortMapperStatus) bool {
		mapping := status.Mapping(ProtocolTcp, 443)
		return mapping != nil && mapping.ExternalPort == 41443
	})
	if status.State != StateMapped {
		t.Fatalf("status = %+v, expected mapped", status)
	}
	if held := gateway.held()[testMappingKey(ProtocolTcp, 443)]; held.externalPort != 41443 {
		t.Fatalf("the router holds %+v, expected the moved port", held)
	}
}

func TestPortMapperRetriesOnABackoffWhenNothingAnswers(t *testing.T) {
	clock := newTestClock()
	gateway := newTestPmpGateway()
	gateway.silent = true
	settings := testPortMapperSettings(clock, gateway, nil)
	mapper := NewPortMapper(context.Background(), testExtenderRequests(), settings)
	defer mapper.Close()

	status := testWaitReady(t, mapper)
	if status.State != StateFailed || len(status.Mappings) != 0 {
		t.Fatalf("status = %+v, expected failed with nothing mapped", status)
	}
	if !strings.Contains(status.Error, "pcp: no answer") || !strings.Contains(status.Error, "upnp: no answer") {
		t.Fatalf("error = %q, expected each method's reason", status.Error)
	}
	for _, expectedWait := range []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute} {
		wait := clock.nextWait(t)
		if wait.wait != expectedWait {
			t.Fatalf("retry wait = %s, expected %s", wait.wait, expectedWait)
		}
		if expectedWait == 20*time.Minute {
			// the router comes back before the third retry
			gateway.set(func() {
				gateway.silent = false
			})
		}
		clock.elapse(wait)
	}
	testWaitStatus(t, mapper, func(status *PortMapperStatus) bool {
		return status.State == StateMapped
	})
	if wait := clock.nextWait(t); wait.wait != 30*time.Minute {
		t.Fatalf("wait after mapping = %s, expected the renewal at half the hour", wait.wait)
	}
}

func TestPortMapperUsesUpnpWhenPcpDoesNotAnswer(t *testing.T) {
	clock := newTestClock()
	upnpGateway := newTestUpnpGateway(t, 1)
	requests := testExtenderRequests()
	mapper := NewPortMapper(context.Background(), requests, testPortMapperSettings(clock, nil, upnpGateway))
	defer mapper.Close()

	status := testWaitReady(t, mapper)
	if status.State != StateMapped || status.Method != MethodUpnp {
		t.Fatalf("status = %+v, expected mapped by upnp", status)
	}
	if status.ExternalIp != testExternalIp {
		t.Fatalf("external ip = %s, expected the router's", status.ExternalIp)
	}
	held := upnpGateway.held()
	if len(held) != len(requests) {
		t.Fatalf("the router holds %v, expected the three requests", held)
	}
	for _, request := range requests {
		mapping, ok := held[strings.ToUpper(request.Protocol)+"/443"]
		if request.InternalPort == 4053 {
			mapping, ok = held["UDP/4053"]
		}
		if !ok || mapping.internalPort != request.InternalPort || mapping.internalClient != "127.0.0.1" {
			t.Fatalf("%s is held as %+v, expected to this host", request, mapping)
		}
		if mapping.leaseSeconds != 3600 || mapping.description != "URnetwork extender" {
			t.Fatalf("%s lease = %d %q, expected an hour, described", request, mapping.leaseSeconds, mapping.description)
		}
	}
	for _, action := range upnpGateway.receivedActions() {
		if action.name != "AddPortMapping" {
			continue
		}
		if action.args["NewInternalClient"] != "127.0.0.1" || action.args["NewRemoteHost"] != "" {
			t.Fatalf("add = %v, expected this host and any remote", action.args)
		}
	}
}

func TestPortMapperTakesAnyPortFromAnIgd2Router(t *testing.T) {
	clock := newTestClock()
	upnpGateway := newTestUpnpGateway(t, 2)
	upnpGateway.takenPorts["TCP/443"] = true
	upnpGateway.anyPort = 40443
	mapper := NewPortMapper(context.Background(), testExtenderRequests(), testPortMapperSettings(clock, nil, upnpGateway))
	defer mapper.Close()

	status := testWaitReady(t, mapper)
	if mapping := status.Mapping(ProtocolTcp, 443); mapping == nil || mapping.ExternalPort != 40443 {
		t.Fatalf("tcp mapping = %+v, expected the port the router reserved", mapping)
	}
	if mapping, ok := upnpGateway.held()["TCP/40443"]; !ok || mapping.internalPort != 443 {
		t.Fatalf("the router holds %v, expected 40443 to 443", upnpGateway.held())
	}
}

func TestPortMapperFailsAnIgd1ConflictAndLeavesNothing(t *testing.T) {
	clock := newTestClock()
	upnpGateway := newTestUpnpGateway(t, 1)
	upnpGateway.takenPorts["TCP/443"] = true
	mapper := NewPortMapper(context.Background(), testExtenderRequests(), testPortMapperSettings(clock, nil, upnpGateway))
	defer mapper.Close()

	status := testWaitReady(t, mapper)
	if status.State != StateFailed || !strings.Contains(status.Error, "maps external port 443 to another host") {
		t.Fatalf("status = %+v, expected the conflict", status)
	}
	// the udp ports it did map go with the method
	if held := upnpGateway.held(); len(held) != 0 {
		t.Fatalf("the router holds %v, expected nothing", held)
	}
}

func TestPortMapperRefusesPermanentUpnpMappings(t *testing.T) {
	clock := newTestClock()
	upnpGateway := newTestUpnpGateway(t, 1)
	upnpGateway.permanentOnly = true
	mapper := NewPortMapper(context.Background(), testExtenderRequests(), testPortMapperSettings(clock, nil, upnpGateway))
	defer mapper.Close()

	status := testWaitReady(t, mapper)
	if status.State != StateFailed || !strings.Contains(status.Error, "only permanent mappings") {
		t.Fatalf("status = %+v, expected the permanent lease refused", status)
	}
	for _, action := range upnpGateway.receivedActions() {
		if action.name == "AddPortMapping" && action.args["NewLeaseDuration"] == "0" {
			t.Fatalf("the mapper asked for a permanent mapping: %v", action.args)
		}
	}
	if held := upnpGateway.held(); len(held) != 0 {
		t.Fatalf("the router holds %v, expected nothing", held)
	}
}

func TestPortMapperRefusesADescriptionOffTheGateway(t *testing.T) {
	clock := newTestClock()
	upnpGateway := newTestUpnpGateway(t, 1)
	settings := testPortMapperSettings(clock, nil, upnpGateway)
	// the description is on loopback, and the gateway is elsewhere
	settings.Gateway = func() (netip.Addr, netip.Addr, error) {
		return testGatewayIp, testLocalIp, nil
	}
	mapper := NewPortMapper(context.Background(), testExtenderRequests(), settings)
	defer mapper.Close()

	status := testWaitReady(t, mapper)
	if status.State != StateFailed || !strings.Contains(status.Error, "not on the gateway") {
		t.Fatalf("status = %+v, expected the description refused", status)
	}
	if actions := upnpGateway.receivedActions(); len(actions) != 0 {
		t.Fatalf("actions = %v, expected none", actions)
	}
}

func TestPortMapperRefusesAControlUrlOffTheGateway(t *testing.T) {
	clock := newTestClock()
	upnpGateway := newTestUpnpGateway(t, 1)
	// a description that points its control url elsewhere
	upnpGateway.urlBaseHost = "198.51.100.9:5000"
	mapper := NewPortMapper(context.Background(), testExtenderRequests(), testPortMapperSettings(clock, nil, upnpGateway))
	defer mapper.Close()

	status := testWaitReady(t, mapper)
	if status.State != StateFailed || !strings.Contains(status.Error, "on the gateway") {
		t.Fatalf("status = %+v, expected the control url refused", status)
	}
	if actions := upnpGateway.receivedActions(); len(actions) != 0 {
		t.Fatalf("actions = %v, expected none", actions)
	}
}

func TestPortMapperRemovesEveryPcpMappingWhenItCloses(t *testing.T) {
	clock := newTestClock()
	gateway := newTestPmpGateway()
	requests := testExtenderRequests()
	mapper := NewPortMapper(context.Background(), requests, testPortMapperSettings(clock, gateway, nil))
	testWaitReady(t, mapper)
	mappedRequests := gateway.mapRequests()

	mapper.Close()
	if held := gateway.held(); len(held) != 0 {
		t.Fatalf("the router holds %v after close, expected nothing", held)
	}
	removals := gateway.mapRequests()[len(mappedRequests):]
	if len(removals) != len(requests) {
		t.Fatalf("removals = %d, expected one per mapping", len(removals))
	}
	for i, removal := range removals {
		if removal.lifetimeSeconds != 0 || removal.nonce != mappedRequests[i].nonce {
			t.Fatalf("removal %d = %+v, expected lifetime zero with the mapping's nonce", i, removal)
		}
	}
	if status := mapper.Status(); status.State != StateClosed || len(status.Mappings) != 0 {
		t.Fatalf("status = %+v, expected closed", status)
	}
	testAssertOwnPortsOnly(t, gateway.mapRequests(), requests)
}

func TestPortMapperRemovesEveryUpnpMappingWhenItCloses(t *testing.T) {
	clock := newTestClock()
	upnpGateway := newTestUpnpGateway(t, 2)
	upnpGateway.takenPorts["TCP/443"] = true
	upnpGateway.anyPort = 40443
	mapper := NewPortMapper(context.Background(), testExtenderRequests(), testPortMapperSettings(clock, nil, upnpGateway))
	testWaitReady(t, mapper)
	if held := upnpGateway.held(); len(held) != 3 {
		t.Fatalf("the router holds %v, expected three", held)
	}

	mapper.Close()
	if held := upnpGateway.held(); len(held) != 0 {
		t.Fatalf("the router holds %v after close, expected nothing", held)
	}
}

// A upnp mapping is named by its external port alone. A router that lost its
// table and gave the port to another host answers the renewal with a conflict:
// the mapper moves to another port and leaves the other host's mapping alone,
// at the renewal and when it closes.
func TestPortMapperNeverRemovesAnotherHostsUpnpMapping(t *testing.T) {
	clock := newTestClock()
	upnpGateway := newTestUpnpGateway(t, 2)
	upnpGateway.anyPort = 40443
	mapper := NewPortMapper(context.Background(), testExtenderRequests(), testPortMapperSettings(clock, nil, upnpGateway))

	testWaitReady(t, mapper)
	anotherHost := testUpnpMapping{internalPort: 443, internalClient: "127.0.0.2", leaseSeconds: 0, description: "another host"}
	upnpGateway.set(func() {
		upnpGateway.mappings["TCP/443"] = anotherHost
		upnpGateway.takenPorts["TCP/443"] = true
	})
	clock.elapse(clock.nextWait(t))
	testWaitStatus(t, mapper, func(status *PortMapperStatus) bool {
		mapping := status.Mapping(ProtocolTcp, 443)
		return mapping != nil && mapping.ExternalPort == 40443
	})
	if held := upnpGateway.held()["TCP/443"]; held != anotherHost {
		t.Fatalf("the other host's mapping is %+v after the renewal, expected it untouched", held)
	}

	mapper.Close()
	held := upnpGateway.held()
	if held["TCP/443"] != anotherHost {
		t.Fatalf("the other host's mapping is %+v after close, expected it untouched", held["TCP/443"])
	}
	if len(held) != 1 {
		t.Fatalf("the router holds %v after close, expected the other host's mapping alone", held)
	}
	for _, action := range upnpGateway.receivedActions() {
		if action.name == "DeletePortMapping" && action.args["NewExternalPort"] == "443" && action.args["NewProtocol"] == "TCP" {
			t.Fatal("the mapper deleted tcp 443, which another host holds")
		}
	}
}

func TestPortMapperRemovesThePartOfAMethodThatFailedTheFirstPort(t *testing.T) {
	clock := newTestClock()
	gateway := newTestPmpGateway()
	// not authorized, for the port the caller needs
	gateway.refusedResultCodes[testMappingKey(ProtocolTcp, 443)] = 2
	mapper := NewPortMapper(context.Background(), testExtenderRequests(), testPortMapperSettings(clock, gateway, nil))
	defer mapper.Close()

	status := testWaitReady(t, mapper)
	if status.State != StateFailed || !strings.Contains(status.Error, "pcp: not authorized") {
		t.Fatalf("status = %+v, expected the pcp refusal", status)
	}
	if held := gateway.held(); len(held) != 0 {
		t.Fatalf("the router holds %v, expected the udp mappings removed", held)
	}
}

func TestPortMapperKeepsTheFirstPortWhenAnotherIsRefused(t *testing.T) {
	clock := newTestClock()
	gateway := newTestPmpGateway()
	gateway.refusedResultCodes[testMappingKey(ProtocolUdp, 4053)] = 8
	mapper := NewPortMapper(context.Background(), testExtenderRequests(), testPortMapperSettings(clock, gateway, nil))
	defer mapper.Close()

	status := testWaitReady(t, mapper)
	if status.State != StateMapped || len(status.Mappings) != 2 {
		t.Fatalf("status = %+v, expected the two other ports mapped", status)
	}
	if status.Mapping(ProtocolUdp, 4053) != nil || !strings.Contains(status.Error, "udp 4053: no resources") {
		t.Fatalf("status = %+v, expected udp 4053 refused with why", status)
	}
}

func TestPortMapperFailsBehindAnotherNat(t *testing.T) {
	clock := newTestClock()
	gateway := newTestPmpGateway()
	gateway.externalIp = netip.MustParseAddr("10.0.0.2")
	mapper := NewPortMapper(context.Background(), testExtenderRequests(), testPortMapperSettings(clock, gateway, nil))
	defer mapper.Close()

	status := testWaitReady(t, mapper)
	if status.State != StateFailed || !strings.Contains(status.Error, "not public") {
		t.Fatalf("status = %+v, expected the double nat", status)
	}
	if held := gateway.held(); len(held) != 0 {
		t.Fatalf("the router holds %v, expected nothing", held)
	}
}

func TestPortMapperReportsNoGateway(t *testing.T) {
	clock := newTestClock()
	gateway := newTestPmpGateway()
	settings := testPortMapperSettings(clock, gateway, nil)
	settings.Gateway = func() (netip.Addr, netip.Addr, error) {
		return netip.Addr{}, netip.Addr{}, errTestNoRoute
	}
	mapper := NewPortMapper(context.Background(), testExtenderRequests(), settings)
	defer mapper.Close()

	status := testWaitReady(t, mapper)
	if status.State != StateFailed || status.Error != errTestNoRoute.Error() {
		t.Fatalf("status = %+v, expected the gateway error", status)
	}
	if gateway.requestCount != 0 {
		t.Fatalf("the router was sent %d requests, expected none", gateway.requestCount)
	}
}

var errTestNoRoute = testError("the default route is through utun4, a tunnel rather than the router")

type testError string

func (self testError) Error() string {
	return string(self)
}

func TestPublicIpv4(t *testing.T) {
	for _, c := range []struct {
		ip     string
		public bool
	}{
		{ip: "203.0.113.7", public: true},
		{ip: "10.1.2.3", public: false},
		{ip: "192.168.1.1", public: false},
		{ip: "100.64.0.1", public: false},
		{ip: "127.0.0.1", public: false},
		{ip: "169.254.1.1", public: false},
		{ip: "2001:db8::1", public: false},
	} {
		if public := isPublicIpv4(netip.MustParseAddr(c.ip)); public != c.public {
			t.Errorf("%s public = %t, expected %t", c.ip, public, c.public)
		}
	}
}
