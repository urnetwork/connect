package connect

import (
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// net_alt.go -- the alt service as a client reaches it (EXTENDER.md L1 to L4).
//
// Alt is a full connect node and an api server in one process on the proxy
// hosts, with no load balancer in front. It serves H3 on udp 443 and whodis on
// udp 4053 (and on 53 through the router), terminating quic with the real api
// and connect certificates and dispatching by sni.
//
// A client never presents an alt name as sni: it presents the api or connect
// name, which is what the certificates are for, so an alt url decides only
// where the packets go. That is why everything here is a host, not a url with
// a resource under it.
//
// One label rule derives the alt url from a platform (connect) or api url, and
// the env prefix and the family suffix ride through the swap untouched:
// `connect.x` gives `alt.x`, `g2-connect.x` gives `g2-alt.x`, `connect-v4.x`
// gives `alt-v4.x`, and `api.x`, `api-v4.x` and `api-v6.x` give `alt.x`,
// `alt-v4.x` and `alt-v6.x`, since alt serves the api names as it serves the
// connect names. A host with no `connect` or `api` label to replace -- an ip
// literal, a single label, a bare space host, any other service -- derives
// nothing, and the caller then runs without alt rather than dialing a guess.
//
// A request carries its family to alt by name: the api alt dialers send a
// request that presents a family name of the alt url's own space to that
// family's alt name (altHostPortForServerName), so an activation posted to
// `api-v4.x` reaches `alt-v4.x`, which carries v4 alone.

// The dns carrier ports (L2). 53 needs privilege to bind and reaches alt
// through the router or the lb; 4053 is the unprivileged port alt, the connect
// dns listener and every extender bind, and only the sn miner binds 53 as
// well. A client dials alt on 53 first, then 4053, and an extender on the
// ports its record lists first, then whichever of 4053 and 53 it does not
// (net_extender_dns_ports.go).
const (
	DefaultDnsPort    = 53
	DefaultWhodisPort = 4053
)

// The public h3 port of alt (L1), which is what the api's alt h3 dialer uses
// when the alt url names no port of its own.
const DefaultAltH3Port = 443

// The service labels of a platform and an api url, and what each becomes on
// alt.
const (
	platformServiceLabel = "connect"
	apiServiceLabel      = "api"
	altServiceLabel      = "alt"
)

// AltUrlFromPlatformUrl derives the alt url of one space from its platform
// url, or from its api url, by the label rule of L3. The result carries the
// url's scheme and explicit port and nothing below the host, since only the
// host is dialed. "" when there is no `connect` or `api` service label to
// replace.
func AltUrlFromPlatformUrl(platformUrl string) string {
	hostName, port := altUrlHostPort(platformUrl)
	if hostName == "" || net.ParseIP(hostName) != nil {
		return ""
	}
	label, domain, ok := strings.Cut(hostName, ".")
	if !ok || label == "" || !strings.Contains(domain, ".") {
		return ""
	}
	// the service label is `[<env>-]connect[-v4|-v6]` or `[<env>-]api[-v4|-v6]`
	labelParts := strings.Split(label, "-")
	serviceIndex := slices.IndexFunc(labelParts, func(labelPart string) bool {
		return labelPart == platformServiceLabel || labelPart == apiServiceLabel
	})
	if serviceIndex < 0 {
		return ""
	}
	labelParts[serviceIndex] = altServiceLabel
	altHostName := fmt.Sprintf("%s.%s", strings.Join(labelParts, "-"), domain)

	scheme := "https"
	// a bare `host:port` parses with the host as its scheme, so a scheme is
	// taken only from a url that names a host after it
	if parsedUrl, err := url.Parse(strings.TrimSpace(platformUrl)); err == nil && parsedUrl.Scheme != "" && parsedUrl.Host != "" {
		scheme = parsedUrl.Scheme
	}
	if 0 < port {
		return fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(altHostName, strconv.Itoa(port)))
	}
	return fmt.Sprintf("%s://%s", scheme, altHostName)
}

// The alt host and explicit port a request that presents `serverName` is
// sent to (L3, L4). A family name of the alt url's own space -- `api-v4.x` or
// `connect-v6.x`, and their `<env>-` forms -- goes to the alt name of its
// family, so an activation posted to `api-v4.x` reaches `alt-v4.x`, which
// carries v4 alone, as the pinned platform transports pair `connect-v4.x` with
// `alt-v4.x` (transport_family.go). Every other name keeps the alt url's own
// host: a family-agnostic name, which that host already serves; a name with no
// service label, or with no server name at all; a name of another space. So
// does an alt url that is not the one the label rule derives for the name's
// space, since it was set explicitly. The port is always the alt url's, so a
// fixture that pins one keeps it.
func altHostPortForServerName(altUrl string, serverName string) (string, int) {
	altHost, altPort := altUrlHostPort(altUrl)
	serverHost, _ := altUrlHostPort(serverName)
	if altHost == "" || serverHost == "" {
		return altHost, altPort
	}
	familyAltHost, _ := altUrlHostPort(AltUrlFromPlatformUrl(serverHost))
	familyAltLabel, domain, _ := strings.Cut(familyAltHost, ".")
	familyAgnosticAltLabel, ok := strings.CutSuffix(familyAltLabel, "-v4")
	if !ok {
		familyAgnosticAltLabel, ok = strings.CutSuffix(familyAltLabel, "-v6")
	}
	if !ok || fmt.Sprintf("%s.%s", familyAgnosticAltLabel, domain) != altHost {
		return altHost, altPort
	}
	return familyAltHost, altPort
}

// The host and explicit port of an alt url. The host is lowercase with no
// trailing dot and is "" when the url names none; the port is 0 when the url
// names none, which leaves each carrier's own default in force. A bare host
// with no scheme is accepted, since that is what a hand-written setting
// usually is.
func altUrlHostPort(altUrl string) (string, int) {
	altUrl = strings.TrimSpace(altUrl)
	if altUrl == "" {
		return "", 0
	}
	parsedUrl, err := url.Parse(altUrl)
	if err != nil {
		return "", 0
	}
	if parsedUrl.Host == "" && !strings.Contains(altUrl, "//") {
		parsedUrl, err = url.Parse("//" + altUrl)
		if err != nil {
			return "", 0
		}
	}
	hostName := strings.ToLower(strings.TrimSuffix(parsedUrl.Hostname(), "."))
	if hostName == "" {
		return "", 0
	}
	port := 0
	if portString := parsedUrl.Port(); portString != "" {
		if parsedPort, err := strconv.Atoi(portString); err == nil && 0 < parsedPort && parsedPort <= 65535 {
			port = parsedPort
		}
	}
	return hostName, port
}

// The dns carrier ports of an alt host in dial order (L2). An alt url that
// names a port pins that one port, which is what an in-process fixture wants;
// otherwise `dnsPort` (0 takes 53) is dialed first and 4053 second.
func altDnsPorts(altPort int, dnsPort int) []int {
	if 0 < altPort {
		return []int{altPort}
	}
	if dnsPort <= 0 {
		dnsPort = DefaultDnsPort
	}
	if dnsPort == DefaultWhodisPort {
		return []int{DefaultWhodisPort}
	}
	return []int{dnsPort, DefaultWhodisPort}
}

// Dedupes and orders dns ports ascending, which puts 53 before 4053 when both
// are offered (L2). Ports out of range are dropped.
func orderedDnsPorts(dnsPorts []int) []int {
	ordered := []int{}
	for _, dnsPort := range dnsPorts {
		if dnsPort <= 0 || 65535 < dnsPort {
			continue
		}
		if !slices.Contains(ordered, dnsPort) {
			ordered = append(ordered, dnsPort)
		}
	}
	slices.Sort(ordered)
	return ordered
}
