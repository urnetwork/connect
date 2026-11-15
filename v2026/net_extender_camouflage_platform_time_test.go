package connect

// The production caller of the camouflage clock's platform source (EXTENDER-UPDATE2
// §3.2): a client strategy reads the Date of a response of the platform's own api
// whose tls chain verified, and nothing else. These run the real strategy entry
// points against a loopback tls server, since what is pinned is what a real
// response carries.

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

// The names of the fixture: the platform's api and a host that is not it. The
// fixture certificate names both, so a chain verifies for either.
const (
	testPlatformApiHost   = "api.platform.example"
	testPlatformOtherHost = "other.example"
)

// How far the fixture's local clock runs behind the server's.
const testPlatformClockSkew = 10 * time.Minute

// A self-signed certificate for the fixture names, and the pool that trusts it.
func newTestPlatformCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: testPlatformApiHost},
		DNSNames:              []string{testPlatformApiHost, testPlatformOtherHost},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// The local tls server every name of the fixture dials: `/` answers with the
// server's own Date, `/redirect` sends the client to the other host and
// `/bad-date` answers with a Date that does not parse.
func newTestPlatformServer(t *testing.T, certificate tls.Certificate, secure bool) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, fmt.Sprintf("https://%s/", net.JoinHostPort(testPlatformOtherHost, requestPort(r))), http.StatusFound)
		case "/bad-date":
			w.Header().Set("Date", "not a date")
			w.Write([]byte("ok"))
		default:
			w.Write([]byte("ok"))
		}
	}))
	if secure {
		server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
		server.StartTLS()
	} else {
		server.Start()
	}
	t.Cleanup(server.Close)
	return server
}

func requestPort(r *http.Request) string {
	_, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		return "443"
	}
	return port
}

// A strategy of one direct route whose every dial reaches address, so the names
// of the fixture stay in the process. It names hosts as the platform's api.
func newTestPlatformStrategy(
	t *testing.T,
	address string,
	tlsConfig *tls.Config,
	hosts []string,
) *ClientStrategy {
	t.Helper()
	return newTestPlatformStrategyWithDialers(t, address, tlsConfig, hosts, true, false)
}

// The same strategy over the given dialers.
func newTestPlatformStrategyWithDialers(
	t *testing.T,
	address string,
	tlsConfig *tls.Config,
	hosts []string,
	normal bool,
	resilient bool,
) *ClientStrategy {
	t.Helper()
	settings := DefaultClientStrategySettings()
	settings.EnableNormal = normal
	settings.EnableResilient = resilient
	settings.TlsConfig = tlsConfig
	settings.RequestTimeout = 20 * time.Second
	settings.ConnectSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network string, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", address)
		},
	}
	settings.PlatformApiHosts = hosts
	strategy := NewClientStrategy(t.Context(), settings)
	t.Cleanup(strategy.Close)
	return strategy
}

// Runs the local clock behind the server's by the skew, with a fresh set of
// offsets, for the rest of the test.
func skewTestPlatformClock(t *testing.T) {
	t.Helper()
	t.Cleanup(Testing_SetExtenderCamouflageLocalClock(func() time.Time {
		return time.Now().Add(-testPlatformClockSkew)
	}))
}

// The offset the next camouflaged hello to an extender with no observation of its
// own would seal with, which is the platform's.
func testPlatformOffset() time.Duration {
	return Testing_ExtenderCamouflageClockOffset(make([]byte, ed25519.PublicKeySize))
}

func requireTestPlatformOffsetCancelsTheSkew(t *testing.T, when string) {
	t.Helper()
	if offset := testPlatformOffset(); offset < testPlatformClockSkew-time.Minute || testPlatformClockSkew+time.Minute < offset {
		t.Fatalf("%s: offset = %s, expected about +%s to cancel the skew", when, offset, testPlatformClockSkew)
	}
}

// A response of the platform's own api over a verified chain corrects the clock, on
// the get path and on the post path, on http/1.1 and h2, and over the normal and the
// resilient dialers, each of which a real client's api call can take. So does a
// refusal: any status of the api is the api's Date, over the same verified chain.
func TestClientStrategyPlatformResponseCorrectsTheCamouflageClock(t *testing.T) {
	certificate, roots := newTestPlatformCertificate(t)
	tlsConfig := &tls.Config{RootCAs: roots}
	for _, c := range []struct {
		name      string
		http2     bool
		resilient bool
		status    int
		post      bool
	}{
		{name: "get"},
		{name: "post", post: true},
		{name: "get over h2", http2: true},
		{name: "post over h2", http2: true, post: true},
		{name: "get over a resilient dialer", resilient: true},
		{name: "post over a resilient dialer", resilient: true, post: true},
		{name: "get over h2 and a resilient dialer", http2: true, resilient: true},
		{name: "a refusal", status: http.StatusForbidden},
	} {
		t.Run(c.name, func(t *testing.T) {
			skewTestPlatformClock(t)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.status != 0 {
					w.WriteHeader(c.status)
				}
				if r.ProtoMajor == 2 != c.http2 {
					t.Errorf("the request came over %s", r.Proto)
				}
				w.Write([]byte("ok"))
			}))
			server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
			server.EnableHTTP2 = c.http2
			server.StartTLS()
			t.Cleanup(server.Close)
			address := server.Listener.Addr().String()
			_, port, err := net.SplitHostPort(address)
			if err != nil {
				t.Fatal(err)
			}
			apiUrl := fmt.Sprintf("https://%s", net.JoinHostPort(testPlatformApiHost, port))
			strategy := newTestPlatformStrategyWithDialers(t, address, tlsConfig, PlatformApiHostNames(apiUrl), !c.resilient, c.resilient)
			if c.post {
				_, err = HttpPostWithStrategyRaw(t.Context(), strategy, apiUrl+"/", []byte("{}"), "")
			} else {
				_, err = HttpGetWithStrategyRaw(t.Context(), strategy, apiUrl+"/", "")
			}
			if c.status == 0 && err != nil {
				t.Fatal(err)
			}
			if c.status != 0 && err == nil {
				t.Fatal("the refusal was not an error")
			}
			requireTestPlatformOffsetCancelsTheSkew(t, c.name)
		})
	}
}

// Nothing unauthenticated moves the clock (§3.2), and nothing from a host that is
// not the platform's api: a plain http response, a tls response whose chain was
// not verified, a verified response of another host, a redirect off the api to
// another host, a Date that does not parse, a failed request, and any response of
// a strategy that names no host.
func TestClientStrategyUnauthenticatedResponseNeverMovesTheCamouflageClock(t *testing.T) {
	certificate, roots := newTestPlatformCertificate(t)
	secureServer := newTestPlatformServer(t, certificate, true)
	plainServer := newTestPlatformServer(t, certificate, false)
	secureAddress := secureServer.Listener.Addr().String()
	plainAddress := plainServer.Listener.Addr().String()
	_, securePort, err := net.SplitHostPort(secureAddress)
	if err != nil {
		t.Fatal(err)
	}
	_, plainPort, err := net.SplitHostPort(plainAddress)
	if err != nil {
		t.Fatal(err)
	}
	apiUrl := fmt.Sprintf("https://%s", net.JoinHostPort(testPlatformApiHost, securePort))
	otherUrl := fmt.Sprintf("https://%s", net.JoinHostPort(testPlatformOtherHost, securePort))
	plainUrl := fmt.Sprintf("http://%s", net.JoinHostPort(testPlatformApiHost, plainPort))
	verified := &tls.Config{RootCAs: roots}
	unverified := &tls.Config{InsecureSkipVerify: true}
	hosts := PlatformApiHostNames(apiUrl)

	cases := []struct {
		name      string
		address   string
		tlsConfig *tls.Config
		hosts     []string
		url       string
		// the request itself must fail, which is what this case is about
		fails bool
	}{
		{name: "plain http", address: plainAddress, tlsConfig: verified, hosts: hosts, url: plainUrl + "/"},
		{name: "tls with no verified chain", address: secureAddress, tlsConfig: unverified, hosts: hosts, url: apiUrl + "/"},
		{name: "a verified response of another host", address: secureAddress, tlsConfig: verified, hosts: hosts, url: otherUrl + "/"},
		{name: "a redirect off the api", address: secureAddress, tlsConfig: verified, hosts: hosts, url: apiUrl + "/redirect"},
		{name: "a date that does not parse", address: secureAddress, tlsConfig: verified, hosts: hosts, url: apiUrl + "/bad-date"},
		{name: "a chain that fails to verify", address: secureAddress, tlsConfig: &tls.Config{RootCAs: x509.NewCertPool()}, hosts: hosts, url: apiUrl + "/", fails: true},
		{name: "a strategy that names no host", address: secureAddress, tlsConfig: verified, hosts: nil, url: apiUrl + "/"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			skewTestPlatformClock(t)
			strategy := newTestPlatformStrategy(t, c.address, c.tlsConfig, c.hosts)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			_, err := HttpGetWithStrategyRaw(ctx, strategy, c.url, "")
			if c.fails && err == nil {
				t.Fatal("the request did not fail")
			}
			if !c.fails && err != nil {
				t.Fatal(err)
			}
			if offset := testPlatformOffset(); offset != 0 {
				t.Fatalf("offset = %s, expected the clock not to move", offset)
			}
		})
	}
}

// The host names of the api urls: normalized as the requests are compared,
// distinct, and with nothing for a url that names no host.
func TestPlatformApiHostNames(t *testing.T) {
	hostNames := PlatformApiHostNames(
		"https://API.Platform.Example.:8443/path",
		"https://api-v4.platform.example",
		"",
		"https://api.platform.example",
	)
	expected := []string{"api.platform.example", "api-v4.platform.example"}
	if !slices.Equal(hostNames, expected) {
		t.Fatalf("host names = %v, expected %v", hostNames, expected)
	}
	if hostNames := PlatformApiHostNames(); len(hostNames) != 0 {
		t.Fatalf("host names of no url = %v", hostNames)
	}
}
