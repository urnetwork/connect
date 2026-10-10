package connect

// net_http_doh_control.go — bootstrap DoH servers a user names for the
// control names.
//
// A client strategy resolves the names of its network space (api, connect,
// extender) only over DoH (net_http_internal_doh.go), and the extender
// bootstrap queries the same servers. A network that blocks the default
// servers therefore leaves a client unable to reach its api at all. A user can
// name DoH servers that work on that network. They are tried ahead of the
// defaults, which stay in the list behind them.
//
// Only `https://<ip literal>/<path>` urls are accepted. A server named by an
// ip needs no name resolution of its own, so naming one never brings back a
// plaintext bootstrap, and the DoH client verifies the server's certificate
// against that ip (an IP SAN), as it does for the defaults. A server can make
// a lookup fail but cannot redirect a connection: the api and connect tls is
// still verified against the original name, and an extender record must
// verify under the root keys.
//
// The presets are `RegionalControlDohUrls` (net_http_doh_regional.go).

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// The error codes of `ControlDohUrlError`, one per thing a user fixes. The sdk
// maps each to a localized message.
const (
	// not a url, or a url the DoH client cannot query: no path, a query,
	// a fragment, user info or a bad port
	ControlDohErrorUrlInvalid = "url_invalid"
	// a scheme other than https, or none
	ControlDohErrorHttpsRequired = "https_required"
	// a host name where an ip literal is needed
	ControlDohErrorIpRequired = "ip_required"
	// more than `ControlDohMaxUrlCount` servers of one family
	ControlDohErrorTooMany = "too_many"
)

// The most bootstrap DoH servers of one family a user can name. The defaults
// add four of each, and a protected lookup races the servers ahead of them.
const ControlDohMaxUrlCount = 8

// What is wrong with a bootstrap DoH url, as a code from the list above.
type ControlDohUrlError struct {
	Code   string
	Detail string
}

// The code, then the detail when there is one.
func (self *ControlDohUrlError) Error() string {
	if self.Detail == "" {
		return fmt.Sprintf("control doh: %s", self.Code)
	}
	return fmt.Sprintf("control doh: %s: %s", self.Code, self.Detail)
}

// The code of a bootstrap DoH url error, or empty for nil and for errors that
// are not url errors.
func ControlDohUrlErrorCode(err error) string {
	var urlErr *ControlDohUrlError
	if errors.As(err, &urlErr) {
		return urlErr.Code
	}
	return ""
}

// Reads one bootstrap DoH server url. It returns the url in the form the DoH
// client queries -- trimmed, a lower case scheme and the ip in its canonical
// form -- and the server's ip, whose family is the list the url belongs to.
func ParseControlDohUrl(dohUrl string) (string, netip.Addr, error) {
	invalid := func(code string, detail string) (string, netip.Addr, error) {
		return "", netip.Addr{}, &ControlDohUrlError{Code: code, Detail: detail}
	}

	trimmed := strings.TrimSpace(dohUrl)
	if trimmed == "" {
		return invalid(ControlDohErrorUrlInvalid, "empty")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return invalid(ControlDohErrorUrlInvalid, err.Error())
	}
	// a url typed without its scheme parses as a path, and is a missing
	// https as far as the user is concerned
	if u.Scheme != "https" {
		return invalid(ControlDohErrorHttpsRequired, u.Scheme)
	}
	if u.Opaque != "" || u.User != nil || u.Host == "" {
		return invalid(ControlDohErrorUrlInvalid, "not an absolute url with a host")
	}
	// the DoH client appends `?dns=` to the url as it stands
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return invalid(ControlDohErrorUrlInvalid, "a query or fragment")
	}
	if strings.Trim(u.Path, "/") == "" {
		return invalid(ControlDohErrorUrlInvalid, "no path")
	}
	addr, err := netip.ParseAddr(u.Hostname())
	if err != nil {
		return invalid(ControlDohErrorIpRequired, u.Hostname())
	}
	// a zone names an interface of one host, which a stored setting cannot
	if addr.Zone() != "" {
		return invalid(ControlDohErrorUrlInvalid, "an ipv6 zone")
	}
	host := addr.String()
	if addr.Is6() {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || 65535 < portNumber {
			return invalid(ControlDohErrorUrlInvalid, "port "+port)
		}
		host = fmt.Sprintf("%s:%d", host, portNumber)
	}
	u.Host = host
	return u.String(), addr.Unmap(), nil
}

// The DoH settings of a client strategy whose user named bootstrap DoH servers:
// the defaults, with the named servers ahead of the default servers of their
// family. With none named it is the defaults.
//
// The named servers are also seeded as the best recent performers
// (`ServerStatsSeed`), because a query fans out in a weighted random order
// under a small concurrency cap rather than in list order. Without the seed a
// lookup on a network that black-holes the defaults could fill every slot
// with them and time out before it reached a named server. The seed decays
// like any score, so after the first minutes the servers that actually answer
// lead.
//
// The urls are used as given: `ParseControlDohUrl` is the rule a caller reads
// them by.
func ControlDohSettings(dohUrlsIpv4 []string, dohUrlsIpv6 []string) *DohSettings {
	settings := DefaultDohSettings()
	if len(dohUrlsIpv4) == 0 && len(dohUrlsIpv6) == 0 {
		return settings
	}
	resolverSettings := settings.DnsResolverSettings
	resolverSettings.RemoteDohUrlsIpv4 = controlDohUrlsFirst(dohUrlsIpv4, resolverSettings.RemoteDohUrlsIpv4)
	resolverSettings.RemoteDohUrlsIpv6 = controlDohUrlsFirst(dohUrlsIpv6, resolverSettings.RemoteDohUrlsIpv6)
	settings.ServerStatsSeed = map[string]float64{}
	for _, dohUrl := range slices.Concat(dohUrlsIpv4, dohUrlsIpv6) {
		settings.ServerStatsSeed[dohUrl] = dohSeedMaxScore
	}
	return settings
}

// The named urls, then the defaults that are not among them.
func controlDohUrlsFirst(dohUrls []string, defaultDohUrls []string) []string {
	merged := slices.Clone(dohUrls)
	for _, defaultDohUrl := range defaultDohUrls {
		if !slices.Contains(merged, defaultDohUrl) {
			merged = append(merged, defaultDohUrl)
		}
	}
	return merged
}

// A copy of DoH settings that names only the built-in servers: the server
// lists of `DefaultDnsResolverSettings`, and no seed, which only ever favors a
// named server. Everything else is kept as given. A strategy that refuses
// custom DoH servers (`ClientStrategySettings.DisableCustomDohServers`) takes
// this in place of any settings it is handed, `ControlDohSettings` included.
func builtInDohServerSettings(settings *DohSettings) *DohSettings {
	if settings == nil {
		return DefaultDohSettings()
	}
	copied := *settings
	defaultResolverSettings := DefaultDnsResolverSettings()
	if settings.DnsResolverSettings == nil {
		copied.DnsResolverSettings = defaultResolverSettings
	} else {
		resolverSettings := *settings.DnsResolverSettings
		resolverSettings.RemoteDohUrlsIpv4 = defaultResolverSettings.RemoteDohUrlsIpv4
		resolverSettings.RemoteDohUrlsIpv6 = defaultResolverSettings.RemoteDohUrlsIpv6
		resolverSettings.LocalDohUrlsIpv4 = defaultResolverSettings.LocalDohUrlsIpv4
		resolverSettings.LocalDohUrlsIpv6 = defaultResolverSettings.LocalDohUrlsIpv6
		resolverSettings.RemoteDnsIpv4 = defaultResolverSettings.RemoteDnsIpv4
		resolverSettings.RemoteDnsIpv6 = defaultResolverSettings.RemoteDnsIpv6
		resolverSettings.LocalDnsIpv4 = defaultResolverSettings.LocalDnsIpv4
		resolverSettings.LocalDnsIpv6 = defaultResolverSettings.LocalDnsIpv6
		copied.DnsResolverSettings = &resolverSettings
	}
	copied.ServerStatsSeed = nil
	return &copied
}
