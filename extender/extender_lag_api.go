package extender

// The lag.dev measurement API (lag.dev/PLAN.md M1 to M7, M10).
//
// lag.dev measures latency, download and upload from a visitor's browser to
// extenders. An extender whose lifecycle holds a measurement certificate (C4)
// serves this API on its existing tcp 443, chosen by sni (M1). The camouflage
// peek already reassembles and parses the ClientHello in front of tls.Server
// (extender_camouflage.go); a hello whose sni is the extender's measurement
// name -- its id under the measurement zone (N1), the name the installed
// certificate was issued for -- is handed, with the bytes the peek read, to the
// measurement server through an in-process listener. Every other hello takes
// the existing path unchanged, so the measurement certificate is presented for
// no other name, and an identity-ca leaf never for a measurement name. The
// certificate is read at every hello, so one that is withdrawn (C9) stops
// being answered to at once.
//
// The measurement server is a net/http server over those connections (M2). It
// offers http/1.1 alone, so a browser opens parallel connections rather than
// multiplexing one; it reads and sets no cookies; every response it writes is
// no-store; and cors answers only the allowed origins, which every websocket
// upgrade is checked against too.
//
// The lifecycle files (extender_lag_cert*.go, extender_lag_report*.go,
// extender_lag_feed*.go) implement the other side of M10: the certificate this
// side presents, the reports it submits and the feed it mounts.

import (
	"bufio"
	"container/list"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// lag.dev/PLAN.md M10, implemented by the lifecycle files

type lagCertificates interface {
	// the measurement names served now and their certificate, or nil when none
	Certificate() (names []string, certificate *tls.Certificate)
}

type lagReports interface {
	Submit(report *protocol.LagReport)
}

// The paths of the API (M4, M5, M6) and of the feed the lifecycle serves over
// it (M9).
const (
	lagApiSessionPath = "/lag/v1/session"
	lagApiDownPath    = "/lag/v1/down"
	lagApiUpPath      = "/lag/v1/up"
	lagApiFeedPath    = "/ur/feed/v1"
)

// The most request header bytes the measurement server reads. A ticket in the
// session url is a few hundred bytes, and no cookie is ever set for the host.
const lagApiMaxHeaderByteCount = 16 * 1024

// How long a kept-alive measurement connection may sit between requests. A
// browser's streams each hold their own connection, and none is reused for a
// later load, so an idle one is only a slot held (M2).
const lagApiIdleTimeout = 5 * time.Second

// The most sessions one source subnet holds open at once, and the most loads
// it runs at once (M3).
const (
	lagApiMaxSessionsPerSubnet = 16
	lagApiMaxLoadsPerSubnet    = 1
)

// A source's block: the /24 of a v4 source and the /48 of a v6 one, the
// prefixes the api's own limits on guests and tracks key on (A1, A6), and
// never narrower than the admission subnet. A block spans many subnets -- a
// /48 holds 256 /56s -- so the subnet's limit alone would let one block hold
// every session an extender has. A block holds at most a share of
// max_sessions, an eighth, and never less than one subnet's limit: 64 of the
// default 512 (M3).
const (
	lagApiBlockIpv4PrefixBitCount = 24
	lagApiBlockIpv6PrefixBitCount = 48
	lagApiBlockSessionsShare      = 8
)

// The most measurement connections one source subnet holds at once: its 16
// sessions (M3), one load's streams each way, and a few left idle or opened
// again after a refusal. It keeps one subnet from pinning the connection slots
// every carrier shares (M2).
const lagApiMaxConnectionsPerSubnet = 32

// ExtenderLagSettings are the extender's settings for the measurement API, the
// `measurement:` block of lag.dev/PLAN.md M10, plus the two things admission
// needs from whoever runs the extender (M3).
type ExtenderLagSettings struct {
	// Enabled serves the API whenever the lifecycle has a certificate
	// installed. The extender's owner turns it off with
	// `measurement.enabled: false` (M3); a hello for a measurement name then
	// takes the existing path like any other.
	Enabled bool
	// The page origins the API answers (M2): cors echoes an origin only when
	// it is one of these, and every websocket upgrade is refused unless its
	// Origin is one. An origin is `https://<host>[:<port>]`; a dev origin --
	// plain http or a loopback host -- is kept only in a test build
	// (extender_lag_api_dev.go).
	AllowedOrigins []string
	// Sessions open at once (M3). A session that only pings costs a few
	// hundred bytes a second. When it is full, the extender closes its oldest
	// session whose load has ended and whose report has gone out, else its
	// oldest that has been open 15 minutes without starting a load, to admit
	// a new one, and refuses only when it has neither. One source block, a
	// /24 or a /48, holds at most an eighth of it. <= 0 takes the default.
	MaxSessions int
	// Loads running at once (M3); a load past it is refused as busy and its
	// session goes on pinging. <= 0 takes the default.
	ConcurrentLoads int
	// Bytes loads may move in one UTC day, both directions together (M3).
	// 0, the default, is no budget: providers have no budget of any kind
	// today, so a load is never refused or cut for its bytes (decision 18,
	// Q6). An operator who sets one gets loads past it refused as budget, and
	// a load under way cut as a cap; the day's count is kept in memory.
	DailyBudgetBytes int64

	// lag.dev/PLAN.md M3, M10: the trust admission checks a ticket against,
	// which the plan's settings block cannot carry. RootPublicKeys returns the
	// root keys the extender trusts for records -- the directory's
	// (connect.ExtenderDirectory.RootKeys) -- and is read at each admission,
	// so a rotation applies at once; nil admits no ticket. NetworkHosts are the
	// network hosts a ticket may name, the hosts whose records the directory
	// accepts; empty admits no ticket.
	RootPublicKeys func() []ed25519.PublicKey
	NetworkHosts   []string
}

func DefaultExtenderLagSettings() ExtenderLagSettings {
	return ExtenderLagSettings{
		Enabled:          true,
		AllowedOrigins:   []string{"https://lag.dev", "https://ur.io"},
		MaxSessions:      512,
		ConcurrentLoads:  2,
		DailyBudgetBytes: 0,
	}
}

// Whether this build keeps dev origins in the allowed list (M2). Only a test
// build sets it (extender_lag_api_dev.go, and the package's own tests).
var lagApiDevOriginsAllowed = false

// The canonical form of one allowed origin, and whether it is kept (M2): an
// absolute https origin with a host and nothing after it, or in a test build a
// plain http or loopback one. Browsers send the scheme and host in lower case
// and omit a default port, so the canonical form lowers the case and drops a
// trailing slash and an explicit default port.
func lagApiCanonicalOrigin(origin string) (string, bool) {
	origin = strings.TrimSuffix(strings.TrimSpace(origin), "/")
	originUrl, err := url.Parse(origin)
	if err != nil || originUrl.Host == "" || originUrl.User != nil ||
		originUrl.Path != "" || originUrl.RawQuery != "" || originUrl.Fragment != "" {
		return "", false
	}
	scheme := strings.ToLower(originUrl.Scheme)
	host := strings.ToLower(originUrl.Hostname())
	port := originUrl.Port()
	switch {
	case scheme == "https" && port == "443":
		port = ""
	case scheme == "http" && port == "80":
		port = ""
	}
	dev := scheme == "http" || host == "localhost" || strings.HasSuffix(host, ".localhost")
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		dev = true
	}
	switch {
	case scheme != "https" && scheme != "http":
		return "", false
	case dev && !lagApiDevOriginsAllowed:
		return "", false
	}
	hostPort := host
	if strings.Contains(host, ":") {
		hostPort = "[" + host + "]"
	}
	if port != "" {
		hostPort += ":" + port
	}
	return scheme + "://" + hostPort, true
}

// The measurement API of one extender (M1 to M7). Safe for concurrent use.
type extenderLagApi struct {
	server       *ExtenderServer
	settings     ExtenderLagSettings
	certificates lagCertificates
	// counts every change to the installed certificate, nil when the
	// lifecycle has none to watch (C4, C9)
	certificateChanges *connect.MonitorValue[uint64]
	reports            lagReports
	// the lifecycle's feed over websocket (M9); nil answers 404
	feedHandler http.Handler

	identityPrivateKey ed25519.PrivateKey
	identityPublicKey  ed25519.PublicKey
	allowedOrigins     map[string]bool
	// the clock skew a ticket's expiry is allowed, the one records are (M3)
	ticketExpireSkew time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	listener   *lagApiListener
	httpServer *http.Server
	tlsConfig  *tls.Config
	upgrader   *websocket.Upgrader

	budget *lagApiBudget

	// the timings of sessions (M4, M8); tests shorten them
	timings lagApiTimings
	// whether an upload is verified read by read rather than sampled (M6),
	// and the sample choice when it is not; tests fix both
	exactVerification bool
	sampleMessage     func() bool

	stateLock sync.Mutex
	closed    bool
	serving   bool
	sessions  map[lagApiSessionId]*lagApiSession
	// the sessions admitted so far, which orders them by age for a full
	// extender to reclaim the oldest first (M3)
	sessionAdmitCount uint64
	// the ids of admitted tickets, kept until their tickets expire (M3), in
	// the order they were admitted
	usedTickets     map[string]*list.Element
	usedTicketOrder *list.List
	// loads running now (M3)
	loadCount int
	// closed and replaced when a session is admitted, which starts the check
	// of the sessions' name (C9)
	sessionAdmitted chan struct{}
	// the measurement connections each source subnet holds now (M2), and the
	// most it may; tests lower the bound
	subnetConnectionCounts  map[extenderSubnetHash]int
	maxConnectionsPerSubnet int
	// the sessions and loads each source subnet holds now (M3), and the most
	// it may
	subnetSessionCounts  map[extenderSubnetHash]int
	subnetLoadCounts     map[extenderSubnetHash]int
	maxSessionsPerSubnet int
	maxLoadsPerSubnet    int
	// the sessions each source block holds now (M3), and the most it may
	blockSessionCounts  map[extenderSubnetHash]int
	maxSessionsPerBlock int
}

// Builds the measurement API of a server (M10), the API side's one wiring
// function. Nil when the API cannot serve: it is turned off, there is no
// lifecycle to present a certificate, or the extender has no identity key to
// sign a report with. The identity key and the header budget are the server's.
// certificateChanges, when the lifecycle has one, counts every change to the
// installed certificate, so open sessions close the moment their name stops
// being served (C4, C9); without it they close at the next check.
func newExtenderLagApi(
	server *ExtenderServer,
	lagSettings ExtenderLagSettings,
	certificates lagCertificates,
	reports lagReports,
	feedHandler http.Handler,
	certificateChanges *connect.MonitorValue[uint64],
) *extenderLagApi {
	settings := server.settings
	if !lagSettings.Enabled || certificates == nil || len(settings.IdentityKeySeed) == 0 {
		return nil
	}
	identityPrivateKey, err := connect.ExtenderPrivateKeyFromSeed(settings.IdentityKeySeed)
	if err != nil {
		return nil
	}
	lagSettings.AllowedOrigins = slices.Clone(lagSettings.AllowedOrigins)
	allowedOrigins := map[string]bool{}
	for _, origin := range lagSettings.AllowedOrigins {
		if canonicalOrigin, ok := lagApiCanonicalOrigin(origin); ok {
			allowedOrigins[canonicalOrigin] = true
		} else {
			log.Printf("[extender] lag: allowed origin %q is not kept in this build", origin)
		}
	}

	ctx, cancel := context.WithCancel(server.ctx)
	self := &extenderLagApi{
		server:             server,
		settings:           lagSettings,
		certificates:       certificates,
		certificateChanges: certificateChanges,
		reports:            reports,
		feedHandler:        feedHandler,
		identityPrivateKey: identityPrivateKey,
		identityPublicKey:  identityPrivateKey.Public().(ed25519.PublicKey),
		allowedOrigins:     allowedOrigins,
		ticketExpireSkew:   connect.DefaultExtenderDirectorySettings().RecordExpireSkew,
		ctx:                ctx,
		cancel:             cancel,
		listener:           newLagApiListener(),
		timings:            defaultLagApiTimings(),
		exactVerification:  lagApiAesHardware,
		sampleMessage:      lagApiSampleMessage,
		sessions:           map[lagApiSessionId]*lagApiSession{},
		usedTickets:        map[string]*list.Element{},
		usedTicketOrder:    list.New(),
		sessionAdmitted:    make(chan struct{}),

		subnetConnectionCounts:  map[extenderSubnetHash]int{},
		maxConnectionsPerSubnet: lagApiMaxConnectionsPerSubnet,
		subnetSessionCounts:     map[extenderSubnetHash]int{},
		subnetLoadCounts:        map[extenderSubnetHash]int{},
		maxSessionsPerSubnet:    lagApiMaxSessionsPerSubnet,
		maxLoadsPerSubnet:       lagApiMaxLoadsPerSubnet,
		blockSessionCounts:      map[extenderSubnetHash]int{},
	}
	defaultSettings := DefaultExtenderLagSettings()
	if self.settings.MaxSessions <= 0 {
		self.settings.MaxSessions = defaultSettings.MaxSessions
	}
	self.maxSessionsPerBlock = max(lagApiMaxSessionsPerSubnet, self.settings.MaxSessions/lagApiBlockSessionsShare)
	if self.settings.ConcurrentLoads <= 0 {
		self.settings.ConcurrentLoads = defaultSettings.ConcurrentLoads
	}
	// a budget of 0 or less is no budget, never the default's: it is what
	// the default is (decision 18)
	self.settings.NetworkHosts = slices.Clone(self.settings.NetworkHosts)
	self.budget = newLagApiBudget(self.settings.DailyBudgetBytes)
	self.tlsConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		// http/1.1 alone, so a browser opens a connection per stream rather
		// than multiplexing one (M1)
		NextProtos:     []string{"http/1.1"},
		GetCertificate: self.getCertificate,
	}
	protocols := &http.Protocols{}
	protocols.SetHTTP1(true)
	headerTimeout := settings.HeaderTimeout
	if headerTimeout <= 0 {
		headerTimeout = DefaultExtenderSettings().HeaderTimeout
	}
	self.httpServer = &http.Server{
		Handler: self,
		// the handshake and each request on the header budget of
		// EXTENDER.md A9. No request has a body, so the whole request is read
		// within it; a download clears its read deadline and sets its own
		// write deadline, since it outlives any fixed timeout, and a
		// websocket sets its own once it is taken over (M2)
		ReadHeaderTimeout: headerTimeout,
		ReadTimeout:       headerTimeout,
		IdleTimeout:       lagApiIdleTimeout,
		MaxHeaderBytes:    lagApiMaxHeaderByteCount,
		Protocols:         protocols,
		TLSNextProto:      map[string]func(*http.Server, *tls.Conn, http.Handler){},
		// a measurement host is public, and a browser that leaves mid
		// handshake is ordinary; nothing here is worth a log line
		ErrorLog: log.New(io.Discard, "", 0),
		// every request ends with the api, hijacked websockets included
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}
	self.upgrader = &websocket.Upgrader{
		HandshakeTimeout: headerTimeout,
		// the api checks the origin itself (M2): a stream's before its
		// upgrade, and a session's after, since a session is refused in band
		// so the page can read why (M4)
		CheckOrigin: func(*http.Request) bool {
			return true
		},
		Error: func(w http.ResponseWriter, r *http.Request, status int, reason error) {
			w.WriteHeader(status)
		},
	}
	return self
}

// The certificate of a measurement handshake (M1): the installed one, for one
// of its names only.
func (self *extenderLagApi) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	names, certificate := self.certificates.Certificate()
	if certificate == nil || !lagApiNameIn(names, hello.ServerName) {
		return nil, errLagApiNoCertificate
	}
	return certificate, nil
}

var errLagApiNoCertificate = errors.New("no measurement certificate for the name")

var errLagApiMeasurementName = errors.New("a measurement name is never answered with an identity leaf")

// The certificate of a carrier handshake the measurement dispatch did not take
// -- a udp carrier's, or a tcp hello for a name the API is not serving -- which
// is the identity-ca leaf of the existing path, refused for a measurement name
// (M1).
func (self *ExtenderServer) carrierCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if self.measurementName(hello.ServerName) {
		return nil, errLagApiMeasurementName
	}
	return self.certificates.GetCertificate(hello)
}

// Whether a name is a measurement name (N1, M1): any name under the
// measurement zone, whether or not the API is on or serves it, or one the API
// serves now. No carrier gives one an identity leaf, so a measurement name the
// API is not serving fails its handshake where an unknown name would be
// answered, unless the camouflage splices it as it splices any unknown name.
func (self *ExtenderServer) measurementName(serverName string) bool {
	if serverName == "" {
		return false
	}
	if zone := self.measurementZone; zone != "" && strings.HasSuffix(strings.ToLower(serverName), "."+zone) {
		return true
	}
	return self.lag.servesName(serverName)
}

// The measurement zone of a server (N1): its lifecycle's, when it keeps a
// measurement certificate, and otherwise the default, so even an extender
// with the API off never gives a name under the zone an identity leaf (M1).
func lagApiMeasurementZone(lifecycleSettings *ExtenderLagLifecycleSettings) string {
	zone := ""
	if lifecycleSettings != nil && lifecycleSettings.Certificate != nil {
		zone = lifecycleSettings.Certificate.Zone()
	}
	zone = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(zone), "."))
	if zone == "" {
		zone = connect.ExtenderMeasurementZone
	}
	return zone
}

// Whether a server name is one of the measurement names. Names compare without
// case, as dns names do.
func lagApiNameIn(names []string, serverName string) bool {
	if serverName == "" {
		return false
	}
	for _, name := range names {
		if strings.EqualFold(name, serverName) {
			return true
		}
	}
	return false
}

// Whether the API serves a name now, read from the certificate as installed
// at this moment (M1). Nil-safe.
func (self *extenderLagApi) servesName(serverName string) bool {
	if self == nil {
		return false
	}
	return self.serves(serverName)
}

// Whether the accept path should peek the ClientHello for this API (M1): only
// while a certificate is installed for a name to serve. Nil-safe, so the
// accept path asks whether or not the API was built.
func (self *extenderLagApi) peeks() bool {
	if self == nil {
		return false
	}
	names, certificate := self.certificates.Certificate()
	return certificate != nil && 0 < len(names)
}

// dispatch serves a connection whose ClientHello named a measurement name
// (M1). The peeked bytes are replayed into a tls server with the measurement
// certificate and http/1.1 alone, which the in-process listener hands to the
// measurement server; dispatch holds the connection until that server is done
// with it, so the accept path's accounting and shutdown cover it for its whole
// life. Only a name an installed certificate serves now is the API's: every
// other name, one under the measurement zone included, is left to the
// existing path unchanged (false), which splices it like any unknown name when
// splicing is on and otherwise closes it, since no carrier gives a zone name
// an identity leaf. Closing a zone name here would tell a prober which
// extenders keep a measurement zone, gated and canary ones included.
func (self *extenderLagApi) dispatch(ctx context.Context, conn net.Conn, readBytes []byte, serverName string) bool {
	if self == nil || serverName == "" {
		return false
	}
	names, certificate := self.certificates.Certificate()
	if certificate == nil || !lagApiNameIn(names, serverName) {
		return false
	}
	if !self.start() {
		return true
	}
	// a subnet holds a bounded share of the connection slots (M2)
	if !self.beginSubnetConnection(conn.RemoteAddr()) {
		return true
	}
	defer self.endSubnetConnection(conn.RemoteAddr())
	requestConn := newConnWithInitialBytes(conn, readBytes, serverName)
	if !self.listener.push(ctx, tls.Server(requestConn, self.tlsConfig)) {
		requestConn.Close()
		return true
	}
	select {
	case <-requestConn.Closed():
	case <-ctx.Done():
		requestConn.Close()
	}
	return true
}

// Takes one of a source subnet's measurement connections (M2), false when it
// holds its share already. The subnet is the extender's admission subnet
// (EXTENDER.md A12), and an unlimited source or one with no address is not
// counted.
func (self *extenderLagApi) beginSubnetConnection(remoteAddr net.Addr) bool {
	subnetHash, ok := self.subnetOf(remoteAddressString(remoteAddr))
	if !ok {
		return true
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if 0 < self.maxConnectionsPerSubnet && self.maxConnectionsPerSubnet <= self.subnetConnectionCounts[subnetHash] {
		return false
	}
	self.subnetConnectionCounts[subnetHash] += 1
	return true
}

func (self *extenderLagApi) endSubnetConnection(remoteAddr net.Addr) {
	subnetHash, ok := self.subnetOf(remoteAddressString(remoteAddr))
	if !ok {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if count := self.subnetConnectionCounts[subnetHash] - 1; 0 < count {
		self.subnetConnectionCounts[subnetHash] = count
	} else {
		delete(self.subnetConnectionCounts, subnetHash)
	}
}

// The admission subnet of a source (EXTENDER.md A12), false for a source the
// per-subnet limits do not apply to: an unlimited one, or one with no address.
func (self *extenderLagApi) subnetOf(remoteAddress string) (extenderSubnetHash, bool) {
	admission := self.server.admission
	if admission.unlimitedSource(remoteAddress) {
		return extenderSubnetHash{}, false
	}
	return admission.subnetHashOf(remoteAddress)
}

// The block of a source (M3): its /24 or /48, or its admission subnet when an
// operator set that wider. False for the sources subnetOf is false for.
func (self *extenderLagApi) blockOf(remoteAddress string) (extenderSubnetHash, bool) {
	admission := self.server.admission
	if admission.unlimitedSource(remoteAddress) {
		return extenderSubnetHash{}, false
	}
	return extenderPrefixHashOf(
		remoteAddress,
		min(lagApiBlockIpv4PrefixBitCount, admission.ipv4PrefixBitCount),
		min(lagApiBlockIpv6PrefixBitCount, admission.ipv6PrefixBitCount),
	)
}

// Starts the measurement server on first use, as a worker the extender joins
// at CloseAndWait. False once the api or the extender is closing.
func (self *extenderLagApi) start() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return false
	}
	if self.serving {
		return true
	}
	if !self.server.beginWorker() {
		return false
	}
	if !self.server.beginWorker() {
		self.server.endWorker()
		return false
	}
	self.serving = true
	go func() {
		defer self.server.endWorker()
		connect.HandleError(func() {
			self.httpServer.Serve(self.listener)
		})
	}()
	go func() {
		defer self.server.endWorker()
		connect.HandleError(self.watchCertificate)
	}()
	return true
}

// Whether the API serves a name now: the installed certificate names it (M1).
// Read at every request, so a name withdrawn or opted out of (C4, C9) is not
// answered even on a connection opened before.
func (self *extenderLagApi) serves(serverName string) bool {
	names, certificate := self.certificates.Certificate()
	return certificate != nil && lagApiNameIn(names, serverName)
}

// Closes the sessions of a name the API no longer serves (C4 opting out, C9
// withdrawal): at once when the lifecycle's change monitor says the
// certificate changed, and in any case within the certificate check while a
// session is open, which also covers an expiry and a change no monitor saw.
// Nothing runs while no session is open but the wait.
func (self *extenderLagApi) watchCertificate() {
	checkTimer := time.NewTimer(self.timings.certificateCheck)
	checkTimer.Stop()
	defer checkTimer.Stop()
	for {
		// subscribe before reading the certificate, so a change in between
		// wakes the wait below
		var changed chan struct{}
		if self.certificateChanges != nil {
			_, changed = self.certificateChanges.Get()
		}
		sessionCount, sessionAdmitted := self.closeUnservedSessions()
		var checkC <-chan time.Time
		if 0 < sessionCount {
			checkTimer.Reset(self.timings.certificateCheck)
			checkC = checkTimer.C
		}
		select {
		case <-self.ctx.Done():
			return
		case <-changed:
		case <-sessionAdmitted:
		case <-checkC:
		}
		checkTimer.Stop()
	}
}

// Ends every open session whose name the API no longer serves, as its dot
// going away: a close of 1001, which a page reads as the extender gone rather
// than as an error of its own, and a running load ended with it (C9). Returns
// the sessions open before, and the channel that says the next is admitted.
func (self *extenderLagApi) closeUnservedSessions() (int, chan struct{}) {
	names, certificate := self.certificates.Certificate()
	unserved := []*lagApiSession{}
	sessionCount, sessionAdmitted := func() (int, chan struct{}) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for _, session := range self.sessions {
			if certificate == nil || !lagApiNameIn(names, session.serverName) {
				unserved = append(unserved, session)
			}
		}
		return len(self.sessions), self.sessionAdmitted
	}()
	for _, session := range unserved {
		session.end(lagApiCloseCodeAway, "withdrawn", protocol.LagEndReason_LAG_END_ERROR)
	}
	return sessionCount, sessionAdmitted
}

// Ends every session and connection of the API. Idempotent. A websocket
// handler ends with the api context, which its request context derives from.
func (self *extenderLagApi) close() {
	if self == nil {
		return
	}
	sessions := func() []*lagApiSession {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.closed = true
		sessions := make([]*lagApiSession, 0, len(self.sessions))
		for _, session := range self.sessions {
			sessions = append(sessions, session)
		}
		return sessions
	}()
	self.cancel()
	self.listener.Close()
	self.httpServer.Close()
	for _, session := range sessions {
		session.close()
	}
}

// Serves one request (M2). Every response is no-store and varies by origin;
// an allowed origin is echoed for cors and resource timing, and no response
// ever allows credentials. No cookie is read or set. No request has a body:
// one that declares one is refused, and every refusal closes its connection,
// so nothing waits on bytes a client declared and never sends.
func (self *extenderLagApi) ServeHTTP(responseWriter http.ResponseWriter, r *http.Request) {
	w := &lagApiResponseWriter{ResponseWriter: responseWriter}
	header := w.Header()
	header.Set("Cache-Control", "no-store")
	header.Add("Vary", "Origin")

	// a request is an operation of the extender, joined at CloseAndWait
	if !self.server.beginWorker() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	defer self.server.endWorker()

	// a declared body, which no request to the api has: refused before
	// anything could wait for it (M2). net/http still drains a body when the
	// request finishes, up to the read deadline; with the deadline moved to
	// now, the connection closes as soon as the refusal is out.
	if r.ContentLength != 0 || 0 < len(r.TransferEncoding) {
		w.WriteHeader(http.StatusBadRequest)
		http.NewResponseController(w).SetReadDeadline(time.Now())
		return
	}
	// a name withdrawn or opted out of since the connection was opened is
	// not answered (C4, C9): misdirected, and the connection closed
	if r.TLS == nil || !self.serves(r.TLS.ServerName) {
		w.WriteHeader(http.StatusMisdirectedRequest)
		return
	}

	origin, originAllowed := self.requestOrigin(r)
	if originAllowed {
		header.Set("Access-Control-Allow-Origin", origin)
		header.Set("Timing-Allow-Origin", origin)
	}

	switch r.URL.Path {
	case lagApiSessionPath, lagApiDownPath, lagApiUpPath, lagApiFeedPath:
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
	case http.MethodOptions:
		// a preflight, which a simple get never needs: answered for an
		// allowed origin with get alone and no request headers
		if !originAllowed {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		header.Set("Access-Control-Allow-Methods", "GET")
		header.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		header.Set("Allow", "GET, OPTIONS")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	switch r.URL.Path {
	case lagApiSessionPath:
		self.serveSession(w, r, originAllowed)
	case lagApiDownPath:
		self.serveDown(w, r, originAllowed)
	case lagApiUpPath:
		self.serveUp(w, r, originAllowed)
	case lagApiFeedPath:
		self.serveFeed(w, r, originAllowed)
	}
}

// The request's origin and whether it is allowed (M2). A request with no
// Origin, or more than one, is not from an allowed page.
func (self *extenderLagApi) requestOrigin(r *http.Request) (string, bool) {
	origins := r.Header.Values("Origin")
	if len(origins) != 1 {
		return "", false
	}
	origin := origins[0]
	canonicalOrigin, ok := lagApiCanonicalOrigin(origin)
	if !ok || !self.allowedOrigins[canonicalOrigin] {
		return "", false
	}
	return origin, true
}

// The feed over websocket (M9), served by the lifecycle. The origin is checked
// here like every other upgrade's (M2), so the lifecycle's upgrader need not
// know the allowed list.
func (self *extenderLagApi) serveFeed(w http.ResponseWriter, r *http.Request, originAllowed bool) {
	if self.feedHandler == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if !originAllowed {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	self.feedHandler.ServeHTTP(w, r)
}

// Upgrades a request to a websocket whose 101 also carries no-store (M2). The
// upgrader writes its own refusal when the request is not a websocket
// handshake.
func (self *extenderLagApi) upgrade(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	conn, err := self.upgrader.Upgrade(w, r, http.Header{
		"Cache-Control": []string{"no-store"},
	})
	if err != nil {
		self.server.reportError("lag upgrade", err)
		return nil, err
	}
	return conn, nil
}

// The response writer of every request to the api (M2): any response that is
// not a success closes its connection, so a refused client cannot keep it
// open, and net/http never waits to discard a body it declared. It passes the
// take over a websocket needs, and the deadlines and flush a download needs.
type lagApiResponseWriter struct {
	http.ResponseWriter
}

func (self *lagApiResponseWriter) WriteHeader(status int) {
	if status < 200 || 300 <= status {
		self.Header().Set("Connection", "close")
	}
	self.ResponseWriter.WriteHeader(status)
}

func (self *lagApiResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(self.ResponseWriter).Hijack()
}

func (self *lagApiResponseWriter) Flush() {
	http.NewResponseController(self.ResponseWriter).Flush()
}

func (self *lagApiResponseWriter) Unwrap() http.ResponseWriter {
	return self.ResponseWriter
}

// The in-process listener the dispatch feeds the measurement server through
// (M1, M2).
type lagApiListener struct {
	conns     chan net.Conn
	closeOnce sync.Once
	closed    chan struct{}
}

func newLagApiListener() *lagApiListener {
	return &lagApiListener{
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
	}
}

func (self *lagApiListener) Accept() (net.Conn, error) {
	select {
	case conn := <-self.conns:
		return conn, nil
	case <-self.closed:
		return nil, net.ErrClosed
	}
}

// Hands one connection to the server. False when the listener closed or the
// context ended first, in which case the caller still owns the connection.
func (self *lagApiListener) push(ctx context.Context, conn net.Conn) bool {
	select {
	case self.conns <- conn:
		return true
	case <-self.closed:
		return false
	case <-ctx.Done():
		return false
	}
}

func (self *lagApiListener) Close() error {
	self.closeOnce.Do(func() {
		close(self.closed)
	})
	return nil
}

func (self *lagApiListener) Addr() net.Addr {
	return streamAddr{network: "tcp", address: "lag"}
}
