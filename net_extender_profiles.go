package connect

import (
	mathrand "math/rand"

	"golang.org/x/exp/maps"
)

// randomly enumerate up to n extender profiles
func EnumerateExtenderProfiles(n int, visited map[ExtenderProfile]bool) []ExtenderProfile {
	out := map[ExtenderProfile]bool{}

	maxIterations := 32 * n
	for i := 0; len(out) < n && i < maxIterations; i += 1 {
		var hosts []string
		var portConnectModes map[int][]ExtenderConnectMode

		// each persona is equally weighted
		// TODO UDP
		switch mathrand.Intn(2) {
		case 0:
			hosts = mailHosts
			portConnectModes = MailPorts
		default:
			hosts = serviceHosts
			portConnectModes = ServicePorts
		}

		ports := maps.Keys(portConnectModes)
		port := ports[mathrand.Intn(len(ports))]
		connectModes := portConnectModes[port]
		connectMode := connectModes[mathrand.Intn(len(connectModes))]

		var profile ExtenderProfile
		switch connectMode {
		// TODO Udp does not use a host
		default:
			host := hosts[mathrand.Intn(len(hosts))]
			fragment := mathrand.Intn(2) != 0
			reorder := mathrand.Intn(2) != 0
			profile = ExtenderProfile{
				ConnectMode: connectMode,
				ServerName:  host,
				Port:        port,
				Fragment:    fragment,
				Reorder:     reorder,
			}
		}
		if _, ok := visited[profile]; !ok {
			if _, ok := out[profile]; !ok {
				out[profile] = true
			}
		}
	}

	return maps.Keys(out)
}

var ServicePorts = map[int][]ExtenderConnectMode{
	// https and secure dns
	443: []ExtenderConnectMode{ExtenderConnectModeTcpTls, ExtenderConnectModeQuic},
	// dns
	853: []ExtenderConnectMode{ExtenderConnectModeTcpTls},
	// ldap
	636: []ExtenderConnectMode{ExtenderConnectModeTcpTls},
	// docker
	2376: []ExtenderConnectMode{ExtenderConnectModeTcpTls, ExtenderConnectModeQuic},
	// ldap
	3269: []ExtenderConnectMode{ExtenderConnectModeTcpTls},
	// ntp, nts
	4460: []ExtenderConnectMode{ExtenderConnectModeTcpTls},
}

var MailPorts = map[int][]ExtenderConnectMode{
	// imap
	993: []ExtenderConnectMode{ExtenderConnectModeTcpTls},
	// pop
	995: []ExtenderConnectMode{ExtenderConnectModeTcpTls},
	// smtp
	465: []ExtenderConnectMode{ExtenderConnectModeTcpTls},
}

// FIXME load from a bit masked sni-service.gz. We don't want any url fragments to be in the binary
var serviceHosts = []string{}

// FIXME load from bit masked sni-mail.gz. We don't want any url fragments to be in the binary
var mailHosts = []string{}
