package connect

import (
	"context"
	"strings"
	"time"
	// "crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	// "net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"

	"golang.org/x/net/idna"
	// "golang.org/x/exp/maps"
)

func DefaultDohSettings() *DohSettings {
	return &DohSettings{
		ConnectSettings: *DefaultConnectSettings(),
	}
}

// see:
// https://developers.cloudflare.com/1.1.1.1/ip-addresses/
// https://www.quad9.net/
// https://support.opendns.com/hc/en-us/articles/360038086532-Using-DNS-over-HTTPS-DoH-with-OpenDNS
func dohUrlsIpv4() []string {
	return []string{
		"https://1.1.1.1/dns-query",
		"https://1.0.0.1/dns-query",
		"https://9.9.9.9:5053/dns-query",
		"https://149.112.112.112:5053/dns-query",
		"https://208.67.222.222/dns-query",
		"https://208.67.220.220/dns-query",
	}
}

func dohUrlsIpv6() []string {
	return []string{
		"https://[2606:4700:4700::1111]/dns-query",
		"https://[2606:4700:4700::1001]/dns-query",
		"https://[2620:fe::fe]:5053/dns-query",
		"https://[2620:fe::9]:5053/dns-query",
		"https://[2620:119:35::35]/dns-query",
		"https://[2620:119:53::53]/dns-query",
	}
}

type DohSettings struct {
	ConnectSettings
}

type DohCache struct {
	httpClient *http.Client
	settings   *DohSettings

	stateLock             sync.Mutex
	queryResultExpiration map[DohKey]map[netip.Addr]time.Time
}

func NewDohCache(settings *DohSettings) *DohCache {
	httpClient := &http.Client{
		Timeout: settings.RequestTimeout,
		Transport: &http.Transport{
			DialContext:         settings.DialContext,
			TLSHandshakeTimeout: settings.TlsTimeout,
			TLSClientConfig:     settings.TlsConfig,
		},
	}

	return &DohCache{
		httpClient:            httpClient,
		settings:              settings,
		queryResultExpiration: map[DohKey]map[netip.Addr]time.Time{},
	}
}

func (self *DohCache) Query(ctx context.Context, recordType string, domain string) []netip.Addr {
	q := NewDohKey(recordType, domain)

	now := time.Now()

	var ips []netip.Addr
	ok := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		r, ok := self.queryResultExpiration[q]
		if !ok {
			return false
		}
		for ip, expireTime := range r {
			if expireTime.Before(now) {
				return false
			}
			ips = append(ips, ip)
		}
		return true
	}()
	if ok {
		return ips
	}

	ipTtls := DohQueryWithClient(ctx, self.httpClient, 0, q.RecordType, self.settings, q.Domain)
	r := map[netip.Addr]time.Time{}
	for ip, ttlSeconds := range ipTtls {
		r[ip] = now.Add(time.Duration(ttlSeconds) * time.Second)
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		self.queryResultExpiration[q] = r
	}()

	ips = nil
	for ip, _ := range r {
		ips = append(ips, ip)
	}
	return ips
}

func DohQueryWithDefaults(ctx context.Context, recordType string, domains ...string) map[netip.Addr]int {
	return DohQuery(ctx, 0, recordType, DefaultDohSettings(), domains...)
}

// return ip -> ttl (seconds)
// use `ipVersion=0` to try all versions
func DohQuery(ctx context.Context, ipVersion int, recordType string, settings *DohSettings, domains ...string) map[netip.Addr]int {
	httpClient := &http.Client{
		Timeout: settings.RequestTimeout,
		Transport: &http.Transport{
			DialContext:         settings.DialContext,
			TLSHandshakeTimeout: settings.TlsTimeout,
			TLSClientConfig:     settings.TlsConfig,
		},
	}
	defer httpClient.CloseIdleConnections()

	return DohQueryWithClient(
		ctx,
		httpClient,
		ipVersion,
		recordType,
		settings,
		domains...,
	)
}

func DohQueryWithClient(
	ctx context.Context,
	httpClient *http.Client,
	ipVersion int,
	recordType string,
	settings *DohSettings,
	domains ...string,
) map[netip.Addr]int {
	// run all the queries in parallel to all servers

	switch recordType {
	case "A", "AAAA":
	default:
		return map[netip.Addr]int{}
	}

	query := func(dohUrl string, domain string) (result map[netip.Addr]int) {
		result = map[netip.Addr]int{}

		name, err := Punycode(domain)
		if err != nil {
			return
		}

		params := url.Values{}
		params.Add("name", name)
		params.Add("type", recordType)

		requestUrl := fmt.Sprintf("%s?%s", dohUrl, params.Encode())

		request, err := http.NewRequestWithContext(ctx, "GET", requestUrl, nil)
		if err != nil {
			return
		}

		request.Header.Set("Accept", "application/dns-json")
		// note, we do not set the User-Agent for DoH requests
		// see https://bugzilla.mozilla.org/show_bug.cgi?id=1543201#c4

		response, err := httpClient.Do(request)
		if err != nil {
			return
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return
		}

		data, err := io.ReadAll(response.Body)
		if err != nil {
			return
		}

		dohResponse := &DohResponse{}
		err = json.Unmarshal(data, dohResponse)
		if err != nil {
			return
		}

		if dohResponse.Status != 0 {
			return
		}

		// ips := []netip.Addr{}
		for _, answer := range dohResponse.Answer {
			if ip, err := netip.ParseAddr(answer.Data); err == nil {
				// ips = append(ips, ip)
				result[ip] = max(result[ip], answer.TTL)
			}
		}

		return
	}

	var dohUrls []string
	switch ipVersion {
	case 4:
		dohUrls = dohUrlsIpv4()
	case 6:
		dohUrls = dohUrlsIpv6()
	default:
		dohUrls = append(dohUrls, dohUrlsIpv4()...)
		dohUrls = append(dohUrls, dohUrlsIpv6()...)
	}

	out := make(chan map[netip.Addr]int)

	for _, dohUrl := range dohUrls {
		for _, domain := range domains {
			go HandleError(func() {
				ips := query(dohUrl, domain)
				select {
				case out <- ips:
				case <-ctx.Done():
				}
			})
		}
	}

	mergedIps := map[netip.Addr]int{}
	for range dohUrls {
		for range domains {
			select {
			case ips := <-out:
				for ip, ttl := range ips {
					mergedIps[ip] = max(mergedIps[ip], ttl)
				}
			case <-ctx.Done():
			}
		}
	}

	return mergedIps
}

type DohKey struct {
	RecordType string
	Domain     string
}

func NewDohKey(recordType string, domain string) DohKey {
	return DohKey{
		RecordType: strings.ToUpper(recordType),
		Domain:     strings.ToLower(recordType),
	}
}

type DohQuestion struct {
	Name string `json:"name"`
	Type int    `json:"type"`
}

type DohAnswer struct {
	Name string `json:"name"`
	Type int    `json:"type"`
	TTL  int    `json:"TTL"`
	Data string `json:"data"`
}

type DohResponse struct {
	Status   int           `json:"Status"`
	TC       bool          `json:"TC"`
	RD       bool          `json:"RD"`
	RA       bool          `json:"RA"`
	AD       bool          `json:"AD"`
	CD       bool          `json:"CD"`
	Question []DohQuestion `json:"Question"`
	Answer   []DohAnswer   `json:"Answer"`
}

func Punycode(domain string) (string, error) {
	name := strings.TrimSpace(domain)

	return idna.New(
		idna.MapForLookup(),
		idna.Transitional(true),
		idna.StrictDomainName(false),
	).ToASCII(name)
}
