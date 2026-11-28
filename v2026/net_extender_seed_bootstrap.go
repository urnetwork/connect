package connect

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The seed bootstrap pass (EXTENDER-UPDATE2.md §2.3).
//
// A pass fetches the seed artifact from a small configured set of https
// mirrors that can be reached without an extender, verifies what it gets under
// the root keys and network hosts the directory already holds, accepts the
// first acceptable bundle under the sequence rules (ExtenderSeedAcceptor), and
// applies it to the directory (ApplyExtenderSeed). It is what restores reach
// when the dns bootstrap and the control route are both unavailable, so it
// depends on neither:
//
//   - A mirror may carry an address, its dns-independent route: the mirror is
//     dialed there instead of resolving the url's host, and tls still verifies
//     the certificate for the url's host with the normal roots. At least one
//     configured mirror should have one.
//   - A redirect is followed only to a configured destination -- a mirror's
//     host or one of RedirectHosts -- over https, at most MaxRedirectCount
//     times.
//   - Every mirror runs its own attempts, so a mirror that stalls holds up
//     nothing but itself. Attempts, dials and bytes come from one budget, every
//     attempt has its own timeout, failed attempts back off, and the whole
//     pass ends at its deadline or its context, whichever is first. A
//     response is read to at most one byte past the 64 KiB envelope cap.
//   - Nothing fetched is trusted until it verifies. A refused artifact is
//     never retried within a pass, and nothing a mirror serves can change the
//     root keys, the network hosts or the resolver.
//
// After a pass applies a bundle with usable records, the owner wakes the
// network client (ExtenderNetworkClient.Wake): the seed's records are dialed
// like any other verified candidates, and once they give reach, the client
// reads hello and the control state and asks for the gated release as it
// would on any path. A pass never adds an address, so if every seed record is
// expired, revoked or unreachable the result is a bootstrap failure, never a
// dial of an unsigned hint.

// One mirror of the seed artifact.
type ExtenderSeedMirror struct {
	// the https url of the artifact
	Url string
	// The dns-independent route, `ip` or `ip:port`, empty for none. The
	// mirror is dialed here instead of resolving the url's host; tls still
	// verifies the certificate for the url's host. Without a port the url's
	// port is used, else 443.
	Address string
}

type ExtenderSeedBootstrapSettings struct {
	Mirrors []*ExtenderSeedMirror
	// Hosts a mirror may redirect to beyond the mirrors' own hosts, matched
	// exactly and ignoring case. A redirect anywhere else ends that attempt.
	RedirectHosts []string
	// Redirects one attempt follows; zero follows none.
	MaxRedirectCount int

	// The whole pass ends this long after it starts, or at its context's
	// deadline, whichever is first. <= 0 is the default.
	Timeout time.Duration
	// One attempt at one mirror: connect, tls, headers and body, redirects
	// included. <= 0 is the default.
	AttemptTimeout time.Duration
	// Attempts in flight at once over every mirror. <= 0 is the default.
	ParallelCount int
	// The shared budget of the pass: attempts over every mirror, retries
	// included; tcp dials over every attempt, redirects included; and bytes
	// read over every response. <= 0 is the default.
	MaxAttemptCount int
	MaxDialCount    int
	MaxByteCount    int64
	// The backoff before a mirror's next attempt after a failed one, doubling
	// to the max, with jitter. <= 0 is the default.
	MinBackoff time.Duration
	MaxBackoff time.Duration

	// The tcp dial. Nil is a plain net.Dialer, which resolves a host with the
	// system resolver; a mirror with an address is never resolved.
	DialContext DialContextFunction
	// The roots mirror certificates are verified against. Nil is the system
	// roots.
	RootCAs *x509.CertPool
	// The clock a bundle's freshness is judged by. Nil is the directory's.
	Now func() time.Time
}

func DefaultExtenderSeedBootstrapSettings() *ExtenderSeedBootstrapSettings {
	return &ExtenderSeedBootstrapSettings{
		MaxRedirectCount: 2,
		Timeout:          60 * time.Second,
		AttemptTimeout:   15 * time.Second,
		ParallelCount:    4,
		MaxAttemptCount:  12,
		MaxDialCount:     36,
		MaxByteCount:     int64(4 * extenderSeedMaxArtifactByteCount),
		MinBackoff:       1 * time.Second,
		MaxBackoff:       10 * time.Second,
	}
}

// The most mirrors one pass reads; mirrors past it are not tried.
const ExtenderSeedMaxMirrorCount = 16

// The most one mirror response may carry: a serialized envelope at its cap.
// The mirror contract (api/bringyour.yml, `/v1/seed.bin`) is the raw envelope,
// at most 64 KiB, and a reader takes at most one byte past it -- which is how
// it knows a response is too large -- before decoding anything. A mirror may
// serve the text form instead, read the same way, so its bundle must have a
// text that fits; a file or a paste has the text form's own longer cap
// (ExtenderSeedMaxTextByteCount).
const extenderSeedMaxArtifactByteCount = ExtenderSeedMaxEnvelopeByteCount

// The most header bytes a mirror may send.
const extenderSeedMaxResponseHeaderByteCount = 16 * 1024

// What became of one mirror in a pass, one code each. None names an address,
// a url or anything the mirror served.
const (
	// its artifact was accepted, newer than the accepted state
	ExtenderSeedMirrorOutcomeAccepted = "accepted"
	// its artifact was the accepted bundle again
	ExtenderSeedMirrorOutcomeIdentical = "identical"
	// its artifact was fetched and refused; the result's Code says why
	ExtenderSeedMirrorOutcomeRefused = "refused"
	// the mirror is not an https url, its address does not parse, or it is
	// past ExtenderSeedMaxMirrorCount
	ExtenderSeedMirrorOutcomeInvalid = "invalid"
	// no connection
	ExtenderSeedMirrorOutcomeDial = "dial"
	// the tls handshake failed, the certificate included
	ExtenderSeedMirrorOutcomeTls = "tls"
	// an attempt ran out its timeout
	ExtenderSeedMirrorOutcomeTimeout = "timeout"
	// an answer other than 200
	ExtenderSeedMirrorOutcomeHttpStatus = "http_status"
	// a redirect past the bound or to a destination that is not configured
	ExtenderSeedMirrorOutcomeRedirect = "redirect"
	// a response past the artifact cap
	ExtenderSeedMirrorOutcomeTooLarge = "too_large"
	// the shared budget ran out before the mirror finished
	ExtenderSeedMirrorOutcomeBudget = "budget"
	// the pass ended first: another mirror's bundle was accepted, the
	// deadline passed or the pass was canceled
	ExtenderSeedMirrorOutcomeCanceled = "canceled"
	// any other failure to fetch
	ExtenderSeedMirrorOutcomeFetch = "fetch"
)

// One mirror's part in a pass.
type ExtenderSeedMirrorResult struct {
	// the mirror's index in the settings
	Index int
	// one of the ExtenderSeedMirrorOutcome codes
	Outcome string
	// for a refused artifact, the refusal's code (ExtenderSeedErrorCode)
	Code         string
	AttemptCount int
}

// What a pass did.
type ExtenderSeedBootstrapResult struct {
	// the bundle accepted from a mirror, nil when none was
	Bundle *ExtenderSeedBundle
	// ExtenderSeedOutcomeAccepted or ExtenderSeedOutcomeIdentical with a
	// bundle, empty without
	Outcome string
	// what the apply changed, nil without a bundle
	Apply *ExtenderSeedApplyResult
	// one per configured mirror, in the settings' order
	Mirrors []*ExtenderSeedMirrorResult
	// attempts and dials the pass took from its budget, and bytes it read
	AttemptCount int
	DialCount    int
	ByteCount    int64
}

// Runs one pass. The result is always returned, with an outcome for every
// mirror. The error is nil when a bundle was accepted and applied with at
// least one usable record, and otherwise a seed refusal: `no_root_keys` when
// the directory holds none (nothing could verify, so nothing is fetched),
// `no_mirror` when no mirror is usable, `canceled` when `ctx` ended,
// `no_usable_record` when a bundle was accepted but every record in it is
// expired or revoked, and when no bundle was accepted the most telling refusal
// a mirror's artifact got -- a freshness refusal first, so a wrong local clock
// is reported as one -- or `fetch` when no artifact arrived at all.
func ExtenderSeedBootstrap(
	ctx context.Context,
	directory *ExtenderDirectory,
	acceptor *ExtenderSeedAcceptor,
	settings *ExtenderSeedBootstrapSettings,
) (*ExtenderSeedBootstrapResult, error) {
	if directory == nil || acceptor == nil {
		return &ExtenderSeedBootstrapResult{}, fmt.Errorf("extender seed bootstrap needs a directory and an acceptor")
	}
	settings = normalizeExtenderSeedBootstrapSettings(settings, directory)
	pass := newExtenderSeedBootstrapPass(directory, acceptor, settings)
	return pass.run(ctx)
}

func normalizeExtenderSeedBootstrapSettings(
	settings *ExtenderSeedBootstrapSettings,
	directory *ExtenderDirectory,
) *ExtenderSeedBootstrapSettings {
	defaults := DefaultExtenderSeedBootstrapSettings()
	if settings == nil {
		settings = defaults
	}
	copied := *settings
	if copied.Timeout <= 0 {
		copied.Timeout = defaults.Timeout
	}
	if copied.AttemptTimeout <= 0 {
		copied.AttemptTimeout = defaults.AttemptTimeout
	}
	if copied.ParallelCount <= 0 {
		copied.ParallelCount = defaults.ParallelCount
	}
	if copied.MaxAttemptCount <= 0 {
		copied.MaxAttemptCount = defaults.MaxAttemptCount
	}
	if copied.MaxDialCount <= 0 {
		copied.MaxDialCount = defaults.MaxDialCount
	}
	if copied.MaxByteCount <= 0 {
		copied.MaxByteCount = defaults.MaxByteCount
	}
	if copied.MinBackoff <= 0 {
		copied.MinBackoff = defaults.MinBackoff
	}
	if copied.MaxBackoff <= 0 {
		copied.MaxBackoff = defaults.MaxBackoff
	}
	copied.MaxBackoff = max(copied.MaxBackoff, copied.MinBackoff)
	copied.MaxRedirectCount = max(0, copied.MaxRedirectCount)
	if copied.Now == nil {
		copied.Now = directory.settings.Now
	}
	return &copied
}

// One configured mirror as the pass reads it.
type extenderSeedPassMirror struct {
	index int
	url   *url.URL
	// the url's host, lower case, without a port
	host string
	// the port the url names, else 443
	port uint16
	// the dns-independent route, invalid for none
	addrPort netip.AddrPort
}

// The shared budget of one pass.
type extenderSeedPassBudget struct {
	attemptCount atomic.Int64
	dialCount    atomic.Int64
	byteCount    atomic.Int64

	maxAttemptCount int64
	maxDialCount    int64
	maxByteCount    int64
}

func (self *extenderSeedPassBudget) takeAttempt() bool {
	if self.maxAttemptCount < self.attemptCount.Add(1) {
		self.attemptCount.Add(-1)
		return false
	}
	return true
}

func (self *extenderSeedPassBudget) takeDial() bool {
	if self.maxDialCount < self.dialCount.Add(1) {
		self.dialCount.Add(-1)
		return false
	}
	return true
}

// Takes up to `n` bytes, returning how many the budget had.
func (self *extenderSeedPassBudget) takeBytes(n int64) int64 {
	for {
		used := self.byteCount.Load()
		available := self.maxByteCount - used
		if available <= 0 {
			return 0
		}
		take := min(n, available)
		if self.byteCount.CompareAndSwap(used, used+take) {
			return take
		}
	}
}

var (
	errExtenderSeedDialBudget  = errors.New("extender seed: the pass's dial budget is spent")
	errExtenderSeedByteBudget  = errors.New("extender seed: the pass's byte budget is spent")
	errExtenderSeedRedirect    = errors.New("extender seed: a redirect past the bound or to a destination that is not configured")
	errExtenderSeedTooLarge    = errors.New("extender seed: the response is past the artifact cap")
	errExtenderSeedHttpStatus  = errors.New("extender seed: the mirror did not answer 200")
	errExtenderSeedAttemptTime = errors.New("extender seed: the attempt ran out its timeout")
)

// The mirror an attempt is for, which the dial reads to take its
// dns-independent route.
type extenderSeedPassMirrorKey struct{}

// One fetched artifact, or why there is none, handed from a mirror's loop to
// the pass.
type extenderSeedPassFetch struct {
	mirror *extenderSeedPassMirror
	// an attempt was made, whatever came of it
	attempted bool
	artifact  []byte
	outcome   string
}

type extenderSeedBootstrapPass struct {
	directory *ExtenderDirectory
	acceptor  *ExtenderSeedAcceptor
	settings  *ExtenderSeedBootstrapSettings

	budget *extenderSeedPassBudget
	// configured addresses by host, for a redirect to another mirror's host
	hostAddrPorts map[string]netip.AddrPort
	// the destinations a redirect may go to
	redirectHosts map[string]bool
	client        *http.Client
	transport     *http.Transport
}

func newExtenderSeedBootstrapPass(
	directory *ExtenderDirectory,
	acceptor *ExtenderSeedAcceptor,
	settings *ExtenderSeedBootstrapSettings,
) *extenderSeedBootstrapPass {
	self := &extenderSeedBootstrapPass{
		directory: directory,
		acceptor:  acceptor,
		settings:  settings,
		budget: &extenderSeedPassBudget{
			maxAttemptCount: int64(settings.MaxAttemptCount),
			maxDialCount:    int64(settings.MaxDialCount),
			maxByteCount:    settings.MaxByteCount,
		},
		hostAddrPorts: map[string]netip.AddrPort{},
		redirectHosts: map[string]bool{},
	}
	for _, redirectHost := range settings.RedirectHosts {
		if host := strings.ToLower(strings.TrimSpace(redirectHost)); host != "" {
			self.redirectHosts[host] = true
		}
	}
	self.transport = &http.Transport{
		// a mirror is dialed directly, never through a proxy the host names
		Proxy:       nil,
		DialContext: self.dialContext,
		TLSClientConfig: &tls.Config{
			RootCAs:    settings.RootCAs,
			MinVersion: tls.VersionTLS12,
		},
		TLSHandshakeTimeout:    settings.AttemptTimeout,
		ResponseHeaderTimeout:  settings.AttemptTimeout,
		MaxResponseHeaderBytes: extenderSeedMaxResponseHeaderByteCount,
		// every attempt dials its own connection: a connection is never
		// carried from one mirror's route to another's
		DisableKeepAlives:  true,
		DisableCompression: true,
		MaxConnsPerHost:    settings.ParallelCount,
	}
	self.client = &http.Client{
		Transport:     self.transport,
		CheckRedirect: self.checkRedirect,
	}
	return self
}

// Parses the configured mirrors. An invalid one gets its outcome now and is
// not run.
func (self *extenderSeedBootstrapPass) parseMirrors(results []*ExtenderSeedMirrorResult) []*extenderSeedPassMirror {
	mirrors := []*extenderSeedPassMirror{}
	for i, configured := range self.settings.Mirrors {
		mirror, ok := func() (*extenderSeedPassMirror, bool) {
			if configured == nil || ExtenderSeedMaxMirrorCount <= i {
				return nil, false
			}
			mirrorUrl, err := url.Parse(strings.TrimSpace(configured.Url))
			if err != nil || mirrorUrl.Scheme != "https" || mirrorUrl.Hostname() == "" || mirrorUrl.User != nil {
				return nil, false
			}
			port := uint16(443)
			if portString := mirrorUrl.Port(); portString != "" {
				parsedPort, err := strconv.ParseUint(portString, 10, 16)
				if err != nil || parsedPort == 0 {
					return nil, false
				}
				port = uint16(parsedPort)
			}
			mirror := &extenderSeedPassMirror{
				index: i,
				url:   mirrorUrl,
				host:  strings.ToLower(mirrorUrl.Hostname()),
				port:  port,
			}
			if address := strings.TrimSpace(configured.Address); address != "" {
				addrPort, err := parseExtenderSeedMirrorAddress(address, port)
				if err != nil {
					return nil, false
				}
				mirror.addrPort = addrPort
			}
			return mirror, true
		}()
		if !ok {
			results[i].Outcome = ExtenderSeedMirrorOutcomeInvalid
			continue
		}
		mirrors = append(mirrors, mirror)
		self.redirectHosts[mirror.host] = true
		if mirror.addrPort.IsValid() {
			if _, ok := self.hostAddrPorts[mirror.host]; !ok {
				self.hostAddrPorts[mirror.host] = mirror.addrPort
			}
		}
	}
	return mirrors
}

// Reads `ip` or `ip:port`, bracketed for v6 with a port; without a port the
// mirror url's port is used.
func parseExtenderSeedMirrorAddress(address string, defaultPort uint16) (netip.AddrPort, error) {
	if addrPort, err := netip.ParseAddrPort(address); err == nil {
		if addrPort.Port() == 0 {
			return netip.AddrPort{}, fmt.Errorf("port 0")
		}
		return netip.AddrPortFrom(addrPort.Addr().Unmap(), addrPort.Port()), nil
	}
	addr, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(address, "["), "]"))
	if err != nil {
		return netip.AddrPort{}, err
	}
	return netip.AddrPortFrom(addr.Unmap(), defaultPort), nil
}

// The tcp dial of every connection the pass makes. The mirror an attempt is
// for is dialed at its address when it has one; a redirect to another
// configured mirror's host takes that mirror's address; anything else is
// dialed by name. Every dial is taken from the budget.
func (self *extenderSeedBootstrapPass) dialContext(ctx context.Context, network string, address string) (net.Conn, error) {
	if !self.budget.takeDial() {
		return nil, errExtenderSeedDialBudget
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	host = strings.ToLower(host)
	// the configured route of a host is the whole route, port included
	target := net.JoinHostPort(host, port)
	if mirror, ok := ctx.Value(extenderSeedPassMirrorKey{}).(*extenderSeedPassMirror); ok && mirror.addrPort.IsValid() && host == mirror.host {
		target = mirror.addrPort.String()
	} else if addrPort, ok := self.hostAddrPorts[host]; ok {
		target = addrPort.String()
	}
	dialContext := self.settings.DialContext
	if dialContext == nil {
		dialer := &net.Dialer{
			Timeout: self.settings.AttemptTimeout,
		}
		dialContext = dialer.DialContext
	}
	return dialContext(ctx, network, target)
}

func (self *extenderSeedBootstrapPass) checkRedirect(request *http.Request, via []*http.Request) error {
	if self.settings.MaxRedirectCount < len(via) {
		return errExtenderSeedRedirect
	}
	if request.URL.Scheme != "https" || request.URL.User != nil {
		return errExtenderSeedRedirect
	}
	if !self.redirectHosts[strings.ToLower(request.URL.Hostname())] {
		return errExtenderSeedRedirect
	}
	return nil
}

func (self *extenderSeedBootstrapPass) run(ctx context.Context) (*ExtenderSeedBootstrapResult, error) {
	defer self.transport.CloseIdleConnections()

	result := &ExtenderSeedBootstrapResult{
		Mirrors: make([]*ExtenderSeedMirrorResult, len(self.settings.Mirrors)),
	}
	for i := range self.settings.Mirrors {
		result.Mirrors[i] = &ExtenderSeedMirrorResult{Index: i}
	}
	defer func() {
		result.AttemptCount = int(self.budget.attemptCount.Load())
		result.DialCount = int(self.budget.dialCount.Load())
		result.ByteCount = self.budget.byteCount.Load()
	}()

	rootKeys := self.directory.RootKeys()
	networkHosts := self.directory.settings.NetworkHosts
	if rootKeys == nil || rootKeys.Len() == 0 {
		// nothing could verify; fetching would only spend the budget
		return result, newExtenderSeedError(ExtenderSeedCodeNoRootKeys, nil, "")
	}
	mirrors := self.parseMirrors(result.Mirrors)
	if len(mirrors) == 0 {
		return result, newExtenderSeedError(ExtenderSeedCodeNoMirror, nil, "")
	}

	passCtx, passCancel := context.WithTimeout(ctx, self.settings.Timeout)
	defer passCancel()

	fetches := make(chan *extenderSeedPassFetch)
	semaphore := make(chan struct{}, self.settings.ParallelCount)
	var mirrorLoops sync.WaitGroup
	for _, mirror := range mirrors {
		mirrorLoops.Add(1)
		go HandleError(func() {
			defer mirrorLoops.Done()
			self.runMirror(passCtx, mirror, semaphore, fetches)
		})
	}
	go func() {
		mirrorLoops.Wait()
		close(fetches)
	}()

	var refusals []error
	for fetch := range fetches {
		mirrorResult := result.Mirrors[fetch.mirror.index]
		if fetch.attempted {
			mirrorResult.AttemptCount += 1
		}
		if fetch.artifact == nil {
			// a pass that ends during a mirror's backoff keeps the failure the
			// mirror last had, which says more than that the pass ended
			if fetch.outcome != ExtenderSeedMirrorOutcomeCanceled || mirrorResult.Outcome == "" {
				mirrorResult.Outcome = fetch.outcome
			}
			continue
		}
		if result.Bundle != nil {
			// another mirror's bundle was accepted first
			mirrorResult.Outcome = ExtenderSeedMirrorOutcomeCanceled
			continue
		}
		bundle, outcome, err := self.accept(fetch.artifact, rootKeys, networkHosts)
		if err != nil {
			mirrorResult.Outcome = ExtenderSeedMirrorOutcomeRefused
			mirrorResult.Code = ExtenderSeedErrorCode(err)
			refusals = append(refusals, err)
			continue
		}
		mirrorResult.Outcome = outcome
		result.Bundle = bundle
		result.Outcome = outcome
		// the other mirrors stop; their loops drain into this one
		passCancel()
	}

	if result.Bundle != nil {
		apply, err := ApplyExtenderSeed(self.directory, result.Bundle)
		result.Apply = apply
		if err != nil {
			return result, err
		}
		if apply.UsableRecordCount == 0 {
			return result, newExtenderSeedError(
				ExtenderSeedCodeNoUsableRecord,
				nil,
				"every record of the accepted bundle is expired or revoked",
			)
		}
		return result, nil
	}
	if ctx.Err() != nil {
		return result, newExtenderSeedError(ExtenderSeedCodeCanceled, ctx.Err(), "")
	}
	if err := extenderSeedMostTellingRefusal(refusals); err != nil {
		return result, err
	}
	return result, newExtenderSeedError(ExtenderSeedCodeFetch, nil, "no mirror served a bundle before the pass ended")
}

// Parses, verifies and accepts one fetched artifact.
func (self *extenderSeedBootstrapPass) accept(
	artifact []byte,
	rootKeys *ExtenderRootKeySet,
	networkHosts []string,
) (*ExtenderSeedBundle, string, error) {
	envelope, err := ParseExtenderSeedArtifact(artifact)
	if err != nil {
		return nil, "", err
	}
	bundle, err := VerifyExtenderSeed(envelope, rootKeys, networkHosts, self.settings.Now())
	if err != nil {
		return nil, "", err
	}
	outcome, err := self.acceptor.Accept(bundle, rootKeys)
	if err != nil {
		return nil, "", err
	}
	return bundle, outcome, nil
}

// A freshness refusal first, so a wrong local clock is reported as one, then
// the first other refusal.
func extenderSeedMostTellingRefusal(refusals []error) error {
	for _, refusal := range refusals {
		if ExtenderSeedFreshnessError(refusal) {
			return refusal
		}
	}
	if 0 < len(refusals) {
		return refusals[0]
	}
	return nil
}

// One mirror's attempts: fetch, and on a failure that another attempt could
// change, back off and fetch again, until an artifact arrives, the failure is
// final, the budget is spent or the pass ends. Every outcome goes to the pass.
func (self *extenderSeedBootstrapPass) runMirror(
	ctx context.Context,
	mirror *extenderSeedPassMirror,
	semaphore chan struct{},
	fetches chan<- *extenderSeedPassFetch,
) {
	send := func(fetch *extenderSeedPassFetch) {
		// the pass reads every fetch until the last loop ends, so this never
		// waits on a pass that has gone
		fetches <- fetch
	}
	canceled := func() {
		send(&extenderSeedPassFetch{mirror: mirror, outcome: ExtenderSeedMirrorOutcomeCanceled})
	}
	backoff := self.settings.MinBackoff
	for {
		if ctx.Err() != nil {
			canceled()
			return
		}
		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			canceled()
			return
		}
		if ctx.Err() != nil {
			<-semaphore
			canceled()
			return
		}
		if !self.budget.takeAttempt() {
			<-semaphore
			send(&extenderSeedPassFetch{mirror: mirror, outcome: ExtenderSeedMirrorOutcomeBudget})
			return
		}
		artifact, outcome, retry := self.fetch(ctx, mirror)
		<-semaphore
		send(&extenderSeedPassFetch{
			mirror:    mirror,
			attempted: true,
			artifact:  artifact,
			outcome:   outcome,
		})
		if artifact != nil || !retry {
			return
		}

		// +-25 % jitter, so mirrors that failed together do not retry together
		wait := time.Duration(float64(backoff) * (0.75 + 0.5*mathrand.Float64()))
		backoff = min(2*backoff, self.settings.MaxBackoff)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			canceled()
			return
		case <-timer.C:
		}
	}
}

// One attempt at one mirror. It returns the artifact, or the outcome and
// whether another attempt could change it.
func (self *extenderSeedBootstrapPass) fetch(ctx context.Context, mirror *extenderSeedPassMirror) ([]byte, string, bool) {
	attemptCtx, attemptCancel := context.WithTimeoutCause(ctx, self.settings.AttemptTimeout, errExtenderSeedAttemptTime)
	defer attemptCancel()
	attemptCtx = context.WithValue(attemptCtx, extenderSeedPassMirrorKey{}, mirror)

	request, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, mirror.url.String(), nil)
	if err != nil {
		return nil, ExtenderSeedMirrorOutcomeInvalid, false
	}
	request.Header.Set("Accept", "application/octet-stream, text/plain")
	response, err := self.client.Do(request)
	if err != nil {
		outcome, retry := self.classifyFetchError(ctx, attemptCtx, err)
		return nil, outcome, retry
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		// a mirror that is overloaded or mid-update may answer the next
		// attempt; any other answer will not change within a pass
		retry := response.StatusCode == http.StatusTooManyRequests || 500 <= response.StatusCode
		return nil, ExtenderSeedMirrorOutcomeHttpStatus, retry
	}
	if int64(extenderSeedMaxArtifactByteCount) < response.ContentLength {
		return nil, ExtenderSeedMirrorOutcomeTooLarge, false
	}
	artifact, err := self.readArtifact(response.Body)
	if err != nil {
		if errors.Is(err, errExtenderSeedTooLarge) {
			return nil, ExtenderSeedMirrorOutcomeTooLarge, false
		}
		if errors.Is(err, errExtenderSeedByteBudget) {
			return nil, ExtenderSeedMirrorOutcomeBudget, false
		}
		outcome, retry := self.classifyFetchError(ctx, attemptCtx, err)
		return nil, outcome, retry
	}
	return artifact, "", false
}

// Reads the artifact, taking at most one byte past the cap from the body: a
// response that has that byte is too large. Every byte kept is taken from the
// shared budget.
func (self *extenderSeedBootstrapPass) readArtifact(body io.Reader) ([]byte, error) {
	limited := io.LimitReader(body, int64(extenderSeedMaxArtifactByteCount)+1)
	artifact := []byte{}
	buffer := make([]byte, 4096)
	for {
		n, err := limited.Read(buffer)
		if 0 < n {
			if extenderSeedMaxArtifactByteCount < len(artifact)+n {
				return nil, errExtenderSeedTooLarge
			}
			if taken := self.budget.takeBytes(int64(n)); taken < int64(n) {
				return nil, errExtenderSeedByteBudget
			}
			artifact = append(artifact, buffer[:n]...)
		}
		if err == io.EOF {
			return artifact, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// The outcome of a failed request, and whether another attempt could change
// it. A pass that ended is canceled, never retried; a tls failure, a refused
// redirect and a spent budget are final.
func (self *extenderSeedBootstrapPass) classifyFetchError(
	passCtx context.Context,
	attemptCtx context.Context,
	err error,
) (string, bool) {
	switch {
	case passCtx.Err() != nil:
		return ExtenderSeedMirrorOutcomeCanceled, false
	case errors.Is(err, errExtenderSeedRedirect):
		return ExtenderSeedMirrorOutcomeRedirect, false
	case errors.Is(err, errExtenderSeedDialBudget):
		return ExtenderSeedMirrorOutcomeBudget, false
	case errors.Is(context.Cause(attemptCtx), errExtenderSeedAttemptTime), errors.Is(err, context.DeadlineExceeded):
		return ExtenderSeedMirrorOutcomeTimeout, true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ExtenderSeedMirrorOutcomeTimeout, true
	}
	var certVerificationErr *tls.CertificateVerificationError
	var unknownAuthorityErr x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var certificateInvalidErr x509.CertificateInvalidError
	var recordHeaderErr tls.RecordHeaderError
	var alertErr tls.AlertError
	switch {
	case errors.As(err, &certVerificationErr),
		errors.As(err, &unknownAuthorityErr),
		errors.As(err, &hostnameErr),
		errors.As(err, &certificateInvalidErr),
		errors.As(err, &recordHeaderErr),
		errors.As(err, &alertErr):
		return ExtenderSeedMirrorOutcomeTls, false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return ExtenderSeedMirrorOutcomeDial, true
	}
	return ExtenderSeedMirrorOutcomeFetch, true
}
