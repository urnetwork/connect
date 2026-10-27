package extender

// Measurement sessions (lag.dev/PLAN.md M3, M4, M8).
//
// A session is what one ticket admits: a control websocket that stays open
// until the browser closes it, pinged continuously from its hello, with at most
// one load -- a download phase, then an upload phase -- whose streams find the
// session by its id. Sessions and loads are limited separately: a session that
// only pings costs a few hundred bytes a second, while a load moves real bytes,
// so loads are few at a time and draw on a daily budget (M3).
//
// Each session runs one loop that owns its pings, its load and its report, and
// is the only writer of its text messages. The handler's goroutine reads the
// browser's messages and hands them to the loop; the pong handler, which runs
// in that read, stamps each pong as it arrives, so a round trip is measured at
// the extender whatever the loop is doing.
//
// A load ends the way the browser's client expects (M4, S5). When the upload's
// window has run out and is final -- every stream's reads from before its end
// have counted, at most lagSpreadMax after it (amendment 15) -- the browser is
// told with the last progress, says done, and gets the result; a load the
// extender cuts short -- an upload mismatch, a cap, a phase that never starts
// -- gets its result at once. A stream that arrives ahead of the message that
// opens its phase waits for it a moment rather than being refused, which the
// client's retries would also cover.
//
// Each session yields one signed report: the minute around its load, or its
// last minute when it had none (M8).

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// The interval of the load series and the progress messages (M4, M8).
const lagApiInterval = 250 * time.Millisecond

// What the extender applies when a ticket leaves a value unset, and the bounds
// it holds any ticket to. A6 issues a 250 ms ping interval, 5 s phases, 4
// streams and 2 GiB each way.
const (
	lagApiDefaultPingInterval  = 250 * time.Millisecond
	lagApiMinPingInterval      = 100 * time.Millisecond
	lagApiMaxPingInterval      = 10 * time.Second
	lagApiDefaultPhaseDuration = 5 * time.Second
	// a window and its grace fit the intervals a report may carry (A7)
	lagApiMaxPhaseDuration    = lagApiMaxIntervalCount*lagApiInterval - lagApiPhaseGrace
	lagApiDefaultStreamCount  = 4
	lagApiDefaultMaxByteCount = 2 * 1024 * 1024 * 1024
)

// While a load runs, the pings go out back to back, one in flight, as fast as
// the path answers, so the page sees the load's latency in detail
// (lag.dev/PLAN.md amendment 22). This floor keeps a path a few ms long from
// sending the page hundreds of rtt messages a second.
const lagApiLoadPingInterval = 10 * time.Millisecond

// The most bytes one control message from the browser may hold.
const lagApiMaxControlMessageByteCount = 4 * 1024

// The most ticket ids kept as used at once; past it the oldest is forgotten.
const lagApiMaxUsedTicketCount = 64 * 1024

// The pings a session keeps: a minute at the fastest cadence, with room.
const lagApiPingRingCount = 320

// The random payload of each ping (M4): unpredictable, so no client can answer
// a ping before it arrives and have the extender sign a round trip below the
// path's.
const lagApiPingPayloadByteCount = 16

// The most lost pings a session keeps for their late pongs (M4). Pings are
// lost at most once per loss timeout, so a pong timeout holds at most 7 at the
// default timings.
const lagApiMaxLostPingCount = 16

// The timings of a session (M4, M8). Tests shorten them.
type lagApiTimings struct {
	// a ping with no pong within this is lost (M4)
	pingLost time.Duration
	// the session ends when no pong has arrived for this long (M4)
	pongTimeout time.Duration
	// the report's minute runs from reportBefore ahead of the download's first
	// byte to reportAfter past the upload's end, and is never longer than
	// reportWindow (M8)
	reportBefore time.Duration
	reportAfter  time.Duration
	reportWindow time.Duration
	// a phase's first byte must come within this of the browser asking for the
	// phase, and the upload must be asked for within it of the download's end
	phaseWait time.Duration
	// a stream that arrives before the control message of its phase waits
	// this long for it
	streamWait time.Duration
	// while a session is open, how often the API checks that its name is
	// still served, beside the lifecycle's change monitor (C9)
	certificateCheck time.Duration
	// after an upload ran out its window, the result waits this long for the
	// browser's done before it goes anyway (M4)
	doneWait time.Duration
	// what a busy refusal tells the browser to wait (M4)
	busyRetryAfter time.Duration
	// a text message that cannot be written within this ends the session
	writeTimeout time.Duration
	// a session that has started no load in this long since its admission
	// may be closed by a full extender to admit a new one, once no session
	// has finished (M3); see lagApiIdleReclaim
	idleReclaim time.Duration
}

// The idle reclaim wait (M3). A track in view asks its dots for their loads in
// turn, the first at 5 s and each next 15 s after the one before, so its last
// dot loads at about 110 s (L4). A load refused as busy is asked once more
// after the next dot's, a dot that reconnects loads first, and a slow setup
// pushes the line back, which together add a minute or two: no dot of a page
// in view waits more than about 5 minutes. 15 is well clear of that, and of a
// page put away for a few minutes. A holder that only pings then keeps only
// the sessions it opened in the last 15 minutes: at the api's 60 tracks an
// hour per /24 (/48) and one reissue each (A6), 15 to 30 a block, where
// before it could keep every session the extender has.
const lagApiIdleReclaim = 15 * time.Minute

func defaultLagApiTimings() lagApiTimings {
	return lagApiTimings{
		pingLost:         5 * time.Second,
		pongTimeout:      30 * time.Second,
		reportBefore:     25 * time.Second,
		reportAfter:      25 * time.Second,
		reportWindow:     60 * time.Second,
		phaseWait:        10 * time.Second,
		streamWait:       2 * time.Second,
		certificateCheck: 2 * time.Second,
		doneWait:         5 * time.Second,
		busyRetryAfter:   5 * time.Second,
		writeTimeout:     10 * time.Second,
		idleReclaim:      lagApiIdleReclaim,
	}
}

// The id of one session, base64url without padding on the wire (M4).
type lagApiSessionId [16]byte

func newLagApiSessionId() (lagApiSessionId, error) {
	var sessionId lagApiSessionId
	_, err := rand.Read(sessionId[:])
	return sessionId, err
}

func parseLagApiSessionId(text string) (lagApiSessionId, bool) {
	var sessionId lagApiSessionId
	if base64.RawURLEncoding.EncodedLen(len(sessionId)) != len(text) {
		return sessionId, false
	}
	n, err := base64.RawURLEncoding.Decode(sessionId[:], []byte(text))
	if err != nil || n != len(sessionId) {
		return sessionId, false
	}
	return sessionId, true
}

func (self lagApiSessionId) String() string {
	return base64.RawURLEncoding.EncodeToString(self[:])
}

// The session-ending errors of M4 and their close codes.
const (
	lagApiErrorTicket   = "ticket"
	lagApiErrorReused   = "reused"
	lagApiErrorSessions = "sessions"
	lagApiErrorOrigin   = "origin"
	lagApiErrorInternal = "internal"
	// the load refusals, after which the session goes on pinging
	lagApiErrorBusy   = "busy"
	lagApiErrorBudget = "budget"
)

func lagApiErrorCloseCode(code string) int {
	switch code {
	case lagApiErrorTicket, lagApiErrorReused, lagApiErrorOrigin:
		return lagApiCloseCodeRefused
	case lagApiErrorSessions:
		return lagApiCloseCodeLimited
	default:
		return lagApiCloseCodeInternal
	}
}

// The text of an end reason in a loadResult (M4): the enum name in lower case
// without its prefix, so LAG_END_UPLOAD_MISMATCH is "upload_mismatch".
func lagApiEndReasonText(endReason protocol.LagEndReason) string {
	return strings.ToLower(strings.TrimPrefix(endReason.String(), "LAG_END_"))
}

// The messages of M4.

type lagApiHelloMessage struct {
	Type           string `json:"type"`
	Session        string `json:"session"`
	UploadKey      string `json:"uploadKey"`
	PingIntervalMs int64  `json:"pingIntervalMs"`
	IntervalMs     int64  `json:"intervalMs"`
	DownloadMs     int64  `json:"downloadMs"`
	UploadMs       int64  `json:"uploadMs"`
	MaxStreams     int    `json:"maxStreams"`
}

type lagApiRttMessage struct {
	Type string   `json:"type"`
	T    int64    `json:"t"`
	Ms   *float64 `json:"ms,omitempty"`
	Lost bool     `json:"lost,omitempty"`
}

type lagApiUploadedMessage struct {
	Type  string  `json:"type"`
	Bytes uint64  `json:"bytes"`
	Ms    int64   `json:"ms"`
	At    float64 `json:"at"`
}

type lagApiLoadResultMessage struct {
	Type           string `json:"type"`
	UploadVerified bool   `json:"uploadVerified"`
	EndReason      string `json:"endReason"`
	// nil, and so left out, when the upload had no first byte
	UploadBytes []uint64 `json:"uploadBytes,omitzero"`
}

type lagApiErrorMessage struct {
	Type         string `json:"type"`
	Code         string `json:"code"`
	RetryAfterMs *int64 `json:"retryAfterMs,omitempty"`
}

// A message from the browser: a load phase, or download progress (M4).
type lagApiControlMessage struct {
	Type  string  `json:"type"`
	Phase string  `json:"phase"`
	Bytes float64 `json:"bytes"`
	Ms    float64 `json:"ms"`
}

// One pong, stamped where it was read.
type lagApiPong struct {
	appData     string
	arrivalTime time.Time
}

// The ping in flight, or one counted lost (M4): its random payload and when it
// was sent.
type lagApiInFlightPing struct {
	payload  string
	sendTime time.Time
}

// One ping's result (M4, M8).
type lagApiPing struct {
	sendTime time.Time
	rtt      time.Duration
	lost     bool
}

// The recent pings of a session, oldest overwritten first. Owned by the
// session loop.
type lagApiPingRing struct {
	pings [lagApiPingRingCount]lagApiPing
	next  int
	count int
}

func (self *lagApiPingRing) add(ping lagApiPing) {
	self.pings[self.next] = ping
	self.next = (self.next + 1) % len(self.pings)
	self.count = min(self.count+1, len(self.pings))
}

// The pings sent in [startTime, endTime), oldest first.
func (self *lagApiPingRing) between(startTime time.Time, endTime time.Time) []lagApiPing {
	pings := []lagApiPing{}
	for i := 0; i < self.count; i += 1 {
		ping := self.pings[(self.next-self.count+i+len(self.pings))%len(self.pings)]
		if !ping.sendTime.Before(startTime) && ping.sendTime.Before(endTime) {
			pings = append(pings, ping)
		}
	}
	return pings
}

// The download bytes per interval as the browser reported them (M4, M8). Each
// report is cumulative from the browser's first byte; the growth since the one
// before goes to the interval the report's time closes. Every interval a
// report closes is in the series, one that brought nothing new as a zero, as
// the browser's own figures count it, so a quiet end of the window is not left
// out of the signed p50 (amendment 14). It holds no more intervals than the
// download's window has, a report from past the window and half an interval is
// not counted, and nothing is ever past what the extender wrote.
type lagApiReceivedSeries struct {
	counts         [lagApiMaxIntervalCount]uint64
	lastCumulative uint64
	lastIndex      int
}

func (self *lagApiReceivedSeries) add(ms float64, cumulative float64, writtenByteCount uint64, downloadDuration time.Duration) {
	if math.IsNaN(ms) || math.IsInf(ms, 0) || ms < 0 ||
		math.IsNaN(cumulative) || math.IsInf(cumulative, 0) || cumulative < 0 {
		return
	}
	intervalMs := float64(lagApiInterval / time.Millisecond)
	downloadMs := float64(downloadDuration / time.Millisecond)
	// a report from past the window counts for nothing (M4)
	if downloadMs+intervalMs/2 < ms {
		return
	}
	// never past the window's intervals, ceil(downloadMs / intervalMs)
	intervalCount := min(len(self.counts), max(1, int(math.Ceil(downloadMs/intervalMs))))
	index := min(intervalCount-1, max(0, int(math.Round(ms/intervalMs))-1))
	// the interval is in the series whether or not it brought new bytes
	self.lastIndex = max(self.lastIndex, index+1)
	cumulativeByteCount := uint64(min(cumulative, float64(writtenByteCount)))
	if cumulativeByteCount <= self.lastCumulative {
		return
	}
	self.counts[index] += cumulativeByteCount - self.lastCumulative
	self.lastCumulative = cumulativeByteCount
}

func (self *lagApiReceivedSeries) values() []uint64 {
	values := make([]uint64, self.lastIndex)
	copy(values, self.counts[:self.lastIndex])
	return values
}

// The session's one load (M3, M4): a download phase, then an upload phase.
// The phases are set under the session's state lock, since streams look them
// up; everything else is the loop's.
//
// A load settles when its transfer is over -- no more bytes count, its slot is
// free for the next load, and the minute its report covers is fixed -- and
// ends when its result has gone to the browser. An upload that ran out its
// window settles once the window is final (amendment 15), and its result
// follows the browser's done; a load the extender cuts short ends at once.
type lagApiLoad struct {
	requestTime time.Time
	download    *lagApiPhase
	upload      *lagApiPhase

	// the download's end, from which the upload must be asked for
	phaseWaitStarted bool
	// the upload's progress messages have begun
	progressStarted bool
	received        lagApiReceivedSeries
	// a phase reached the ticket's cap or the daily budget
	capped bool
	// the browser said done once the upload had its first byte, before the
	// load settled: its window still open, or run out but not yet final
	doneReceived bool

	settled   bool
	endTime   time.Time
	endReason protocol.LagEndReason
	ended     bool
}

// Whether the upload ran and every message checked was the expected keystream
// (M6).
func (self *lagApiLoad) uploadVerified() bool {
	if self.upload == nil {
		return false
	}
	return self.upload.verified()
}

// One measurement session (M4).
type lagApiSession struct {
	api       *extenderLagApi
	conn      *websocket.Conn
	id        lagApiSessionId
	uploadKey [16]byte

	// the measurement name the control connection was opened for, which the
	// session lasts no longer than (C9)
	serverName string
	ticketId   []byte
	// until when the ticket's id is kept as used: its expiry and the skew (M3)
	ticketExpire  time.Time
	trackId       []byte
	trackPosition uint32
	// the control connection's address, and its admission subnet and block
	// when the per-subnet limits apply to it (M3). All are held in memory for
	// admission only: no report or log carries them (M8, amendment 9). The
	// report names only the address family, 4 or 6.
	remoteAddress string
	ipFamily      uint32
	subnet        extenderSubnetHash
	block         extenderSubnetHash
	subnetLimited bool
	// the order and time of its admission, and whether a load of its has
	// been admitted: so a full extender reclaims the oldest session first,
	// and one that has started no load only once it has been open the idle
	// reclaim wait (M3). Guarded by the api's state lock.
	admitIndex  uint64
	admitTime   time.Time
	loadStarted bool
	// set by the loop once the session's load has ended and its report has
	// gone out, after which it only pings and has nothing left to measure: a
	// full extender may then close it to admit a new session, before any
	// session that has started no load (M3)
	finished atomic.Bool

	pingInterval     time.Duration
	downloadDuration time.Duration
	uploadDuration   time.Duration
	maxStreamCount   int
	maxDownloadBytes int64
	maxUploadBytes   int64

	// done when the session ends, which ends its load
	ctx    context.Context
	cancel context.CancelFunc

	// to the loop: the browser's messages, the pongs, and phase changes
	messages chan lagApiControlMessage
	pongs    chan lagApiPong
	wake     chan struct{}
	// the ping in flight, which the loop sets before it sends and the pong
	// handler matches against; nil when none is (M4)
	inFlight atomic.Pointer[lagApiInFlightPing]
	// the pings counted lost that were sent within the pong timeout, oldest
	// first, which the loop replaces at each loss: a late pong for one of
	// them never counts as a round trip, but it shows the page is still
	// there (M4)
	lostPings atomic.Pointer[[]lagApiInFlightPing]
	// when the last pong that answered a ping sent within the pong timeout
	// arrived, as its time since startTime: set by the pong handler, and read
	// by the loop, which ends the session when none has come for the pong
	// timeout (M4)
	lastPongSinceStart atomic.Int64

	stateLock sync.Mutex
	// the session's load, nil before it; streams read it here
	load *lagApiLoad
	// closed and replaced whenever the load or a phase is set, for a stream
	// that arrives before its phase
	changed chan struct{}
	// the close the extender sends when it ends the session, and how that
	// ends a load still running
	closeCode          int
	closeText          string
	closeLoadEndReason protocol.LagEndReason

	// the loop's own, and the handler's once the loop has returned
	startTime time.Time
	pings     lagApiPingRing
	refused   bool
	reported  bool
}

// Builds a session from an admitted ticket, its values held to the extender's
// bounds.
func newLagApiSession(
	api *extenderLagApi,
	conn *websocket.Conn,
	body *protocol.LagTicketBody,
) (*lagApiSession, error) {
	sessionId, err := newLagApiSessionId()
	if err != nil {
		return nil, err
	}
	var uploadKey [16]byte
	if _, err := rand.Read(uploadKey[:]); err != nil {
		return nil, err
	}
	durationOf := func(ms uint32, defaultDuration time.Duration, minDuration time.Duration, maxDuration time.Duration) time.Duration {
		if ms == 0 {
			return defaultDuration
		}
		return max(minDuration, min(maxDuration, time.Duration(ms)*time.Millisecond))
	}
	byteCountOf := func(byteCount uint64) int64 {
		if byteCount == 0 {
			return lagApiDefaultMaxByteCount
		}
		return int64(min(byteCount, math.MaxInt64))
	}
	maxStreamCount := lagApiDefaultStreamCount
	if body.MaxStreams != 0 {
		maxStreamCount = int(min(body.MaxStreams, lagApiMaxStreamCount))
	}
	ctx, cancel := context.WithCancel(api.ctx)
	return &lagApiSession{
		api:              api,
		conn:             conn,
		id:               sessionId,
		uploadKey:        uploadKey,
		ticketId:         body.TicketId,
		ticketExpire:     time.UnixMilli(int64(min(body.ExpireTimeMs, math.MaxInt64))).Add(api.ticketExpireSkew),
		trackId:          body.TrackId,
		trackPosition:    body.TrackPosition,
		pingInterval:     durationOf(body.PingIntervalMs, lagApiDefaultPingInterval, lagApiMinPingInterval, lagApiMaxPingInterval),
		downloadDuration: durationOf(body.DownloadMs, lagApiDefaultPhaseDuration, lagApiInterval, lagApiMaxPhaseDuration),
		uploadDuration:   durationOf(body.UploadMs, lagApiDefaultPhaseDuration, lagApiInterval, lagApiMaxPhaseDuration),
		maxStreamCount:   maxStreamCount,
		maxDownloadBytes: byteCountOf(body.MaxDownloadBytes),
		maxUploadBytes:   byteCountOf(body.MaxUploadBytes),
		ctx:              ctx,
		cancel:           cancel,
		messages:         make(chan lagApiControlMessage, 16),
		pongs:            make(chan lagApiPong, 4),
		wake:             make(chan struct{}, 1),
		changed:          make(chan struct{}),
	}, nil
}

// GET /lag/v1/session?ticket=<ticket text> (M3, M4). The websocket is upgraded
// before anything is checked, so a refusal reaches the page as an error message
// and a close code it can read.
func (self *extenderLagApi) serveSession(w http.ResponseWriter, r *http.Request, originAllowed bool) {
	conn, err := self.upgrade(w, r)
	if err != nil {
		return
	}
	defer conn.Close()
	if !originAllowed {
		self.refuseSession(conn, lagApiErrorOrigin)
		return
	}
	// an admission is one action of its subnet (M3, EXTENDER.md A12),
	// counted before the ticket is so much as read
	if limit, _ := self.server.admitAction(r.RemoteAddr, false); limit != extenderAdmitted {
		self.server.reportError(extenderAdmissionStage(limit), fmt.Errorf("the source is over its admission limit"))
		self.refuseSession(conn, lagApiErrorSessions)
		return
	}
	body, err := self.verifyTicket(r.URL.Query().Get("ticket"), time.Now())
	if err != nil {
		self.server.reportError("lag ticket", err)
		self.refuseSession(conn, lagApiErrorTicket)
		return
	}
	// a used ticket is refused before anything is built for it, so a replay
	// costs only its check (M3)
	if self.ticketUsed(body.TicketId) {
		self.refuseSession(conn, lagApiErrorReused)
		return
	}
	session, err := newLagApiSession(self, conn, body)
	if err != nil {
		self.server.reportError("lag session", err)
		self.refuseSession(conn, lagApiErrorInternal)
		return
	}
	session.serverName = r.TLS.ServerName
	session.setSource(r.RemoteAddr)
	if code := self.admitSession(session); code != "" {
		// a refused admission releases what it built, the session's context
		// above all, so refusals cannot accumulate (M3)
		session.cancel()
		self.refuseSession(conn, code)
		return
	}
	session.serve()
}

// Sets where the session's control connection comes from, before admission
// (M3): its address, its family for the report (M8), and the admission subnet
// and block its limits count it under, when they apply to it.
func (self *lagApiSession) setSource(remoteAddress string) {
	self.remoteAddress = remoteAddress
	self.ipFamily = lagApiIpFamily(remoteAddress)
	self.subnet, self.subnetLimited = self.api.subnetOf(remoteAddress)
	if self.subnetLimited {
		// the same address at a width no narrower, so it has one too
		self.block, _ = self.api.blockOf(remoteAddress)
	}
}

// Whether a ticket's id is one an admitted session used (M3).
func (self *extenderLagApi) ticketUsed(ticketId []byte) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	_, ok := self.usedTickets[string(ticketId)]
	return ok
}

// Ends a session that was never admitted with its error and close (M4).
func (self *extenderLagApi) refuseSession(conn *websocket.Conn, code string) {
	if messageBytes, err := json.Marshal(&lagApiErrorMessage{Type: "error", Code: code}); err == nil {
		conn.SetWriteDeadline(time.Now().Add(lagApiCloseWait))
		conn.WriteMessage(websocket.TextMessage, messageBytes)
	}
	lagApiCloseWebSocket(conn, lagApiErrorCloseCode(code), code)
}

// The address family of a connection's remote address, 4 or 6, after
// unmapping a v4-mapped address; 0 when it does not parse.
func lagApiIpFamily(remoteAddr string) uint32 {
	addrPort, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return 0
	}
	if addrPort.Addr().Unmap().Is4() {
		return 4
	}
	return 6
}

// Checks a ticket (M3): the root signature under the ticket domain with the
// root keys the extender trusts for records, its expiry, that it names this
// extender, and that its network host is one this extender serves.
func (self *extenderLagApi) verifyTicket(ticketText string, now time.Time) (*protocol.LagTicketBody, error) {
	ticket, err := connect.DecodeLagTicket(ticketText)
	if err != nil {
		return nil, err
	}
	rootPublicKeys := []ed25519.PublicKey{}
	if self.settings.RootPublicKeys != nil {
		rootPublicKeys = self.settings.RootPublicKeys()
	}
	// the expiry with the clock skew records are allowed, so an extender whose
	// clock runs a little fast still admits a ticket issued just now
	body, err := connect.VerifyLagTicket(ticket, rootPublicKeys, now.Add(-self.ticketExpireSkew))
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(body.ExtenderPublicKey, self.identityPublicKey) {
		return nil, fmt.Errorf("the lag ticket names another extender")
	}
	networkHostOk := false
	for _, networkHost := range self.settings.NetworkHosts {
		if strings.EqualFold(networkHost, body.NetworkHost) {
			networkHostOk = true
			break
		}
	}
	if !networkHostOk {
		return nil, fmt.Errorf("the lag ticket is for network %q", body.NetworkHost)
	}
	if len(body.TicketId) != 16 {
		return nil, fmt.Errorf("the lag ticket id is %d bytes", len(body.TicketId))
	}
	return body, nil
}

// The close of a session that a full extender ended to admit a new one (M3,
// M4): 4410, after HTTP's 410 Gone, since the session is over for good. A
// session whose load has ended and whose report has gone out is closed this
// way first, the oldest first, so the dot's results are complete and signed.
// Only when no session has got that far is one closed that has started no
// load in the idle reclaim wait, the oldest first: no load of it is running,
// and none can start once it is chosen. Its one report, its last minute with
// no load, goes out as it ends. The browser keeps what the dot has, stops
// pinging it, and does not reconnect: a new session would only take a slot
// again. It is not 4409, because the API answers a stream that comes before
// its phase with 409, and the browser retries that (M4).
const (
	lagApiCloseCodeReclaimed = 4410
	lagApiCloseTextReclaimed = "reclaimed"
)

// Admits an open session (M3): its ticket unused, room under its subnet's
// limit and its block's, and room under max_sessions. A full extender makes
// room by closing a session (reclaimableSessionWithLock), and refuses the new
// session only when none may be closed. The count is the extender's, so the
// session closed may be any subnet's; the subnet's and the block's limits come
// first, so no session is closed for one that is then refused. The ticket is
// marked used only when the session is admitted, and kept until it expires.
// Returns the error that refuses the session, or "".
func (self *extenderLagApi) admitSession(session *lagApiSession) string {
	now := time.Now()
	ticketId := string(session.ticketId)
	// the session closed to make room ends once the state lock is released,
	// as every session the api ends does: deferred before the lock is taken,
	// this runs after the unlock
	var reclaimed *lagApiSession
	defer func() {
		if reclaimed != nil {
			// no load of it runs -- its load has ended, or it started none and
			// can start none now -- so the load end reason never applies (M8)
			reclaimed.end(lagApiCloseCodeReclaimed, lagApiCloseTextReclaimed, protocol.LagEndReason_LAG_END_ERROR)
		}
	}()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return lagApiErrorInternal
	}
	for {
		oldest := self.usedTicketOrder.Front()
		if oldest == nil {
			break
		}
		usedTicket := oldest.Value.(*lagApiUsedTicket)
		if now.Before(usedTicket.expireTime) && self.usedTicketOrder.Len() < lagApiMaxUsedTicketCount {
			break
		}
		self.usedTicketOrder.Remove(oldest)
		delete(self.usedTickets, usedTicket.ticketId)
	}
	if _, ok := self.usedTickets[ticketId]; ok {
		return lagApiErrorReused
	}
	// the subnet's limit and the block's come before any room is made, so no
	// session is closed for one that is then refused
	if session.subnetLimited && 0 < self.maxSessionsPerSubnet &&
		self.maxSessionsPerSubnet <= self.subnetSessionCounts[session.subnet] {
		return lagApiErrorSessions
	}
	if session.subnetLimited && 0 < self.maxSessionsPerBlock &&
		self.maxSessionsPerBlock <= self.blockSessionCounts[session.block] {
		return lagApiErrorSessions
	}
	if self.settings.MaxSessions <= len(self.sessions) {
		reclaimed = self.reclaimableSessionWithLock(now)
		if reclaimed == nil {
			return lagApiErrorSessions
		}
		// its slot and its subnet's and block's counts go to the new session
		// at once, and it can start no load from here (beginLoad)
		self.removeSessionWithLock(reclaimed)
	}
	self.usedTickets[ticketId] = self.usedTicketOrder.PushBack(&lagApiUsedTicket{
		ticketId:   ticketId,
		expireTime: session.ticketExpire,
	})
	self.sessionAdmitCount += 1
	session.admitIndex = self.sessionAdmitCount
	session.admitTime = now
	self.sessions[session.id] = session
	if session.subnetLimited {
		self.subnetSessionCounts[session.subnet] += 1
		self.blockSessionCounts[session.block] += 1
	}
	close(self.sessionAdmitted)
	self.sessionAdmitted = make(chan struct{})
	return ""
}

// The open session a full extender closes to make room at now (M3), or nil
// when none may be closed. Called with the state lock held.
//   - First, the oldest that has finished: its load has ended, its result is
//     with the browser and its report has gone out, so it has nothing left
//     to measure.
//   - Else the oldest that has started no load and has been open the idle
//     reclaim wait: a holder that only pings, a load refused, a dot's session
//     opened again after its load, or a page put away. Any of them may still
//     ask for a load, which is why one that has finished goes first, and why
//     a young one is never closed: a track in view leaves no dot waiting that
//     long.
//
// A session whose load is running, or whose report's minute is still open,
// is never closed. Neither lasts, and few are at once: a load ends within a
// minute, at most concurrent_loads at a time, and its minute closes
// reportAfter later.
func (self *extenderLagApi) reclaimableSessionWithLock(now time.Time) *lagApiSession {
	var finished *lagApiSession
	var idle *lagApiSession
	for _, session := range self.sessions {
		switch {
		case session.finished.Load():
			if finished == nil || session.admitIndex < finished.admitIndex {
				finished = session
			}
		case !session.loadStarted && self.timings.idleReclaim <= now.Sub(session.admitTime):
			if idle == nil || session.admitIndex < idle.admitIndex {
				idle = session
			}
		}
	}
	if finished != nil {
		return finished
	}
	return idle
}

// One used ticket id and when its ticket expires (M3).
type lagApiUsedTicket struct {
	ticketId   string
	expireTime time.Time
}

func (self *extenderLagApi) removeSession(session *lagApiSession) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeSessionWithLock(session)
}

// Removes an open session and releases its subnet's and block's counts. A
// session already removed, as one reclaimed for a new session is, releases
// nothing again. Called with the state lock held.
func (self *extenderLagApi) removeSessionWithLock(session *lagApiSession) {
	if self.sessions[session.id] != session {
		return
	}
	delete(self.sessions, session.id)
	if session.subnetLimited {
		lagApiReleaseSubnetCount(self.subnetSessionCounts, session.subnet)
		lagApiReleaseSubnetCount(self.blockSessionCounts, session.block)
	}
}

// Releases one of a subnet's or block's counted sessions or loads.
func lagApiReleaseSubnetCount(counts map[extenderSubnetHash]int, subnet extenderSubnetHash) {
	if count := counts[subnet] - 1; 0 < count {
		counts[subnet] = count
	} else {
		delete(counts, subnet)
	}
}

// The open session of an id, or nil.
func (self *extenderLagApi) session(sessionId lagApiSessionId) *lagApiSession {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.sessions[sessionId]
}

// What beginLoad answers a session a full extender has closed to make room
// (M3): it starts no load, and there is nothing to tell the browser, whose
// close is on its way. Never sent.
const lagApiLoadReclaimed = "reclaimed"

// Admits a session's load (M3): refused as budget when today's budget is
// spent, and as busy when the session's subnet is over its admission limit,
// already runs its load, or concurrent_loads are running, each with how long
// to wait. A load is one action of its subnet (EXTENDER.md A12). A session no
// longer open, which only a reclaimed one is while its loop runs, is answered
// lagApiLoadReclaimed: the reclaim and the load are each decided under the
// state lock, so a session chosen for having started no load never starts
// one, and one whose load is admitted is never chosen as idle. Returns ""
// when admitted; the caller then ends it with endLoad.
func (self *extenderLagApi) beginLoad(now time.Time, session *lagApiSession) (string, time.Duration) {
	if self.budget.exhausted(now) {
		return lagApiErrorBudget, self.budget.renewTime(now).Sub(now)
	}
	if limit, retryAfter := self.server.admitAction(session.remoteAddress, false); limit != extenderAdmitted {
		return lagApiErrorBusy, retryAfter
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.sessions[session.id] != session {
		return lagApiLoadReclaimed, 0
	}
	if self.settings.ConcurrentLoads <= self.loadCount {
		return lagApiErrorBusy, self.timings.busyRetryAfter
	}
	if session.subnetLimited && 0 < self.maxLoadsPerSubnet &&
		self.maxLoadsPerSubnet <= self.subnetLoadCounts[session.subnet] {
		return lagApiErrorBusy, self.timings.busyRetryAfter
	}
	self.loadCount += 1
	if session.subnetLimited {
		self.subnetLoadCounts[session.subnet] += 1
	}
	session.loadStarted = true
	return "", 0
}

func (self *extenderLagApi) endLoad(session *lagApiSession) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.loadCount -= 1
	if session.subnetLimited {
		lagApiReleaseSubnetCount(self.subnetLoadCounts, session.subnet)
	}
}

// Serves an admitted session until the browser closes it or the extender ends
// it (M4): the hello, then the loop, then -- once the read side is done -- the
// end of the load and the report (M8).
func (self *lagApiSession) serve() {
	conn := self.conn
	conn.SetReadLimit(lagApiMaxControlMessageByteCount)
	timings := self.api.timings
	// stamped as it is read, so the round trip does not wait on the loop
	conn.SetPongHandler(func(appData string) error {
		arrivalTime := time.Now()
		inFlight := self.inFlight.Load()
		answersInFlight := inFlight != nil && appData == inFlight.payload && !arrivalTime.Before(inFlight.sendTime)
		// any pong that echoes a ping sent within the pong timeout shows the
		// page is still there, one that comes after its ping was counted
		// lost included (M4)
		if answersInFlight || self.answersLostPing(appData, arrivalTime, timings.pongTimeout) {
			self.lastPongSinceStart.Store(int64(arrivalTime.Sub(self.startTime)))
		}
		// only a pong that echoes the ping in flight, arriving after it was
		// sent and within the loss timeout, counts as a round trip (M4): a
		// late, stale, forged or early one is dropped here, so no flood of
		// them can crowd out the one that answers
		if !answersInFlight || timings.pingLost <= arrivalTime.Sub(inFlight.sendTime) {
			return nil
		}
		select {
		case self.pongs <- lagApiPong{appData: appData, arrivalTime: arrivalTime}:
		default:
		}
		return nil
	})
	// the extender's end of the session -- the loop's, or the api closing --
	// sends the close and bounds the drain that follows; joined before the
	// session is done
	closeSent := make(chan struct{})
	stopClose := context.AfterFunc(self.ctx, func() {
		defer close(closeSent)
		code, text := self.closeReason()
		lagApiSendClose(conn, code, text)
	})
	defer func() {
		if !stopClose() {
			<-closeSent
		}
	}()

	self.startTime = time.Now()
	hello := &lagApiHelloMessage{
		Type:           "hello",
		Session:        self.id.String(),
		UploadKey:      base64.RawURLEncoding.EncodeToString(self.uploadKey[:]),
		PingIntervalMs: self.pingInterval.Milliseconds(),
		IntervalMs:     lagApiInterval.Milliseconds(),
		DownloadMs:     self.downloadDuration.Milliseconds(),
		UploadMs:       self.uploadDuration.Milliseconds(),
		MaxStreams:     self.maxStreamCount,
	}
	loopDone := make(chan struct{})
	if self.write(hello) {
		go func() {
			defer close(loopDone)
			connect.HandleError(self.run, self.cancel)
		}()
	} else {
		close(loopDone)
		self.end(lagApiCloseCodeInternal, lagApiErrorInternal, protocol.LagEndReason_LAG_END_ERROR)
	}

	for {
		messageType, reader, err := conn.NextReader()
		if err != nil {
			break
		}
		if messageType != websocket.TextMessage {
			continue
		}
		messageBytes, err := io.ReadAll(reader)
		if err != nil {
			break
		}
		message := lagApiControlMessage{}
		if err := json.Unmarshal(messageBytes, &message); err != nil {
			continue
		}
		select {
		case self.messages <- message:
		case <-self.ctx.Done():
		}
	}

	self.cancel()
	<-loopDone
	self.finish()
}

// Whether a pong echoes a ping counted lost that was sent within the pong
// timeout of the pong's arrival (M4).
func (self *lagApiSession) answersLostPing(appData string, arrivalTime time.Time, pongTimeout time.Duration) bool {
	lostPings := self.lostPings.Load()
	if lostPings == nil {
		return false
	}
	for _, lostPing := range *lostPings {
		if appData == lostPing.payload && arrivalTime.Sub(lostPing.sendTime) <= pongTimeout {
			return true
		}
	}
	return false
}

// Keeps a ping counted lost for the pong timeout, so that its late pong still
// shows the page is there (M4), and lets go of the lost pings sent before
// that. Only the loop calls it; the pong handler reads what it keeps.
func (self *lagApiSession) addLostPing(lostPing lagApiInFlightPing, pongTimeout time.Duration) {
	now := time.Now()
	lostPings := []lagApiInFlightPing{}
	if previousLostPings := self.lostPings.Load(); previousLostPings != nil {
		for _, previousLostPing := range *previousLostPings {
			if now.Sub(previousLostPing.sendTime) <= pongTimeout {
				lostPings = append(lostPings, previousLostPing)
			}
		}
	}
	lostPings = append(lostPings, lostPing)
	if lagApiMaxLostPingCount < len(lostPings) {
		lostPings = lostPings[len(lostPings)-lagApiMaxLostPingCount:]
	}
	self.lostPings.Store(&lostPings)
}

// When the last pong that answered a ping sent within the pong timeout
// arrived, or the session's start until one has (M4).
func (self *lagApiSession) lastPongTime() time.Time {
	return self.startTime.Add(time.Duration(self.lastPongSinceStart.Load()))
}

// Ends the session from the extender's side with a close (M4), and a load
// still running with loadEndReason. The first close wins.
func (self *lagApiSession) end(code int, text string, loadEndReason protocol.LagEndReason) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closeCode == 0 {
			self.closeCode = code
			self.closeText = text
			self.closeLoadEndReason = loadEndReason
		}
	}()
	self.cancel()
}

// How the session's end ends a load still running: as the extender ended it,
// as an error when the api closed, and otherwise as the browser closing it.
func (self *lagApiSession) loadEndReason() protocol.LagEndReason {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	switch {
	case self.closeCode != 0:
		return self.closeLoadEndReason
	case self.api.ctx.Err() != nil:
		return protocol.LagEndReason_LAG_END_ERROR
	default:
		return protocol.LagEndReason_LAG_END_CLIENT_CLOSED
	}
}

// The close the extender sends: the one it ended the session with, else going
// away, which is the api closing.
func (self *lagApiSession) closeReason() (int, string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closeCode == 0 {
		return lagApiCloseCodeAway, ""
	}
	return self.closeCode, self.closeText
}

// Ends the session (M3): from the api's close.
func (self *lagApiSession) close() {
	self.cancel()
}

// Writes one text message, which only the loop does once the session runs. A
// write that fails ends the session.
func (self *lagApiSession) write(message any) bool {
	messageBytes, err := json.Marshal(message)
	if err != nil {
		return false
	}
	self.conn.SetWriteDeadline(time.Now().Add(self.api.timings.writeTimeout))
	if err := self.conn.WriteMessage(websocket.TextMessage, messageBytes); err != nil {
		self.api.server.reportError("lag write", err)
		return false
	}
	return true
}

// Wakes the loop after a phase changed. Never blocks.
func (self *lagApiSession) wakeLoop() {
	select {
	case self.wake <- struct{}{}:
	default:
	}
}

// Makes a change to the load visible to the streams waiting for it.
func (self *lagApiSession) publishWithLock() {
	close(self.changed)
	self.changed = make(chan struct{})
}

// The phase of the direction a stream may open now, waiting up to the stream
// wait when the browser's control message for it has not arrived yet: a stream
// can overtake the message that opens its phase. Nil when the phase is over or
// does not come.
func (self *lagApiSession) waitPhase(ctx context.Context, direction lagApiDirection) *lagApiPhase {
	timer := time.NewTimer(self.api.timings.streamWait)
	defer timer.Stop()
	for {
		phase, wait, changed := self.phaseOf(direction)
		if phase != nil || !wait {
			return phase
		}
		select {
		case <-changed:
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return nil
		case <-self.ctx.Done():
			return nil
		}
	}
}

// The open phase of the direction, else whether one may still come, and the
// channel that says the load changed.
func (self *lagApiSession) phaseOf(direction lagApiDirection) (*lagApiPhase, bool, chan struct{}) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	load := self.load
	if load == nil {
		return nil, true, self.changed
	}
	phase := load.download
	if direction == lagApiDirectionUpload {
		phase = load.upload
		if phase == nil {
			// an upload waits only while the download may still lead to one:
			// one that ran out of time or reached its cap, or is running yet
			_, _, downloadEnd := load.download.state()
			switch downloadEnd {
			case lagApiPhaseRunning, lagApiPhaseEndTime, lagApiPhaseEndCap:
				return nil, true, self.changed
			default:
				return nil, false, nil
			}
		}
	}
	if !phase.running(time.Now()) {
		return nil, false, nil
	}
	return phase, false, nil
}

// The session loop (M4, M8): continuous pings, the load, the progress and the
// report. It returns when the session ends.
func (self *lagApiSession) run() {
	timings := self.api.timings

	// pings: one in flight; the next at the later of the interval after the
	// previous send and the previous pong, or at once after a loss
	pingTimer := time.NewTimer(0)
	defer pingTimer.Stop()
	lossTimer := time.NewTimer(timings.pingLost)
	lossTimer.Stop()
	defer lossTimer.Stop()
	pingC := pingTimer.C
	var lossC <-chan time.Time
	inFlightPayload := ""
	var inFlightSendTime time.Time

	// the load's pings come faster than the ticket's interval; the minute the
	// report carries keeps only those on the ticket's cadence, so it stays
	// inside A7's bounds and the ring's room. A lost ping is always kept.
	var keptPingTime time.Time
	keepPing := func(ping lagApiPing) {
		if !ping.lost && !keptPingTime.IsZero() && ping.sendTime.Sub(keptPingTime) < self.pingInterval-self.pingInterval/8 {
			return
		}
		keptPingTime = ping.sendTime
		self.pings.add(ping)
	}
	loading := func() bool {
		load := self.load
		return load != nil && !load.settled
	}

	// the load: the wait for the upload after the download, the upload's
	// progress, the wait for the browser's done, and the report's minute
	stoppedTimer := func() *time.Timer {
		timer := time.NewTimer(time.Hour)
		timer.Stop()
		return timer
	}
	phaseWaitTimer := stoppedTimer()
	defer phaseWaitTimer.Stop()
	var phaseWaitC <-chan time.Time
	progressTimer := stoppedTimer()
	defer progressTimer.Stop()
	var progressC <-chan time.Time
	doneTimer := stoppedTimer()
	defer doneTimer.Stop()
	var doneC <-chan time.Time
	reportTimer := stoppedTimer()
	defer reportTimer.Stop()
	var reportC <-chan time.Time

	// the upload's progress (M4): the bytes counted in the window so far; ms,
	// how far into the window that is -- its whole length once it is over;
	// and at, when the latest counted read arrived, in ms to the microsecond,
	// which the browser builds its series from (amendment 14). The phase reads
	// them at one moment under its lock (progressReport), so a progress's ms
	// and the phase's state agree. last is the progress that says the window
	// is over, which goes out once the window is final, with the final total;
	// before that no progress reads as over, and a tick too late to tell from
	// it is not sent (amendment 15)
	writeUploaded := func(load *lagApiLoad, last bool) bool {
		byteCount, ms, at, ok := load.upload.progressReport(last)
		if !ok {
			return true
		}
		return self.write(&lagApiUploadedMessage{
			Type:  "uploaded",
			Bytes: byteCount,
			Ms:    ms,
			At:    at,
		})
	}

	// the next progress falls on a whole interval from the first byte, without
	// drift; the window's end sends the last one itself
	scheduleProgress := func(load *lagApiLoad) {
		progressC = nil
		firstByteTime, _, end := load.upload.state()
		if firstByteTime.IsZero() || end != lagApiPhaseRunning {
			return
		}
		next := (time.Since(firstByteTime)/lagApiInterval + 1) * lagApiInterval
		if self.uploadDuration-lagApiInterval/2 <= next {
			return
		}
		progressTimer.Reset(time.Until(firstByteTime.Add(next)))
		progressC = progressTimer.C
	}

	// settles the load (M4, M8): its phases end and their windows are final,
	// its slot is free for the next load, the browser gets the upload's last
	// progress, and the report waits for the close of the load's minute, which
	// runs from the load's end
	settleLoad := func(load *lagApiLoad, endReason protocol.LagEndReason, endTime time.Time) bool {
		load.settled = true
		load.endTime = endTime
		load.endReason = endReason
		load.download.finish(lagApiPhaseEndClosed)
		if load.upload != nil {
			load.upload.finish(lagApiPhaseEndClosed)
		}
		self.api.endLoad(self)
		phaseWaitTimer.Stop()
		phaseWaitC = nil
		progressTimer.Stop()
		progressC = nil
		reportTimer.Reset(time.Until(self.reportEndTime(load)))
		reportC = reportTimer.C
		if load.upload != nil {
			return writeUploaded(load, true)
		}
		return true
	}

	// ends the load with its result to the browser (M4), which, once the
	// upload had its first byte, carries the upload's series exactly as the
	// report will sign it: the load is settled, so its phases have ended and
	// the series is final (amendment 14)
	resultLoad := func(load *lagApiLoad) bool {
		load.ended = true
		doneTimer.Stop()
		doneC = nil
		result := &lagApiLoadResultMessage{
			Type:           "loadResult",
			UploadVerified: load.uploadVerified(),
			EndReason:      lagApiEndReasonText(load.endReason),
		}
		if load.upload != nil {
			if firstByteTime, _, _ := load.upload.state(); !firstByteTime.IsZero() {
				result.UploadBytes = load.upload.windowValues()
			}
		}
		return self.write(result)
	}

	// ends a load the extender cuts short -- a mismatch, a cap, a timeout --
	// with its result at once, which is what tells the browser to stop (M4)
	endLoad := func(load *lagApiLoad, endReason protocol.LagEndReason, endTime time.Time) bool {
		return settleLoad(load, endReason, endTime) && resultLoad(load)
	}

	// ends a load whose upload ran out its window, once the window is final
	// (M4, amendment 15): the last progress tells the browser the window
	// closed, the browser says done, and the result follows; a browser that
	// says nothing gets it after the done wait. endTime is the window's end,
	// as everything but the last progress keys off it.
	completeLoad := func(load *lagApiLoad, endReason protocol.LagEndReason, endTime time.Time) bool {
		if !settleLoad(load, endReason, endTime) {
			return false
		}
		if load.doneReceived {
			return resultLoad(load)
		}
		doneTimer.Reset(timings.doneWait)
		doneC = doneTimer.C
		return true
	}

	// moves the load on after a phase changed (M4)
	reconcile := func() bool {
		load := self.load
		if load == nil || load.settled {
			return true
		}
		_, downloadEndedTime, downloadEnd := load.download.state()
		switch downloadEnd {
		case lagApiPhaseEndTimeout:
			return endLoad(load, protocol.LagEndReason_LAG_END_TIMEOUT, time.Now())
		case lagApiPhaseEndCap:
			load.capped = true
		}
		if load.upload == nil {
			if downloadEnd != lagApiPhaseRunning && !load.phaseWaitStarted {
				load.phaseWaitStarted = true
				phaseWaitTimer.Reset(time.Until(downloadEndedTime.Add(timings.phaseWait)))
				phaseWaitC = phaseWaitTimer.C
			}
			return true
		}
		uploadFirstByteTime, uploadEndedTime, uploadEnd, uploadFinal := load.upload.settlementState()
		switch uploadEnd {
		case lagApiPhaseRunning:
			if !uploadFirstByteTime.IsZero() && !load.progressStarted {
				load.progressStarted = true
				scheduleProgress(load)
			}
		case lagApiPhaseEndTime:
			// reads that arrive past the end still count for what they carry
			// from before it, at most lagSpreadMax after the end (amendment
			// 15). Settlement also waits for those bytes' sampled verification.
			if !uploadFinal {
				return true
			}
			endReason := protocol.LagEndReason_LAG_END_COMPLETE
			if load.capped {
				endReason = protocol.LagEndReason_LAG_END_CAP
			}
			return completeLoad(load, endReason, uploadEndedTime)
		case lagApiPhaseEndCap:
			return endLoad(load, protocol.LagEndReason_LAG_END_CAP, uploadEndedTime)
		case lagApiPhaseEndMismatch:
			return endLoad(load, protocol.LagEndReason_LAG_END_UPLOAD_MISMATCH, uploadEndedTime)
		case lagApiPhaseEndTimeout:
			return endLoad(load, protocol.LagEndReason_LAG_END_TIMEOUT, uploadEndedTime)
		}
		return true
	}

	// a load phase from the browser (M3, M4)
	handleLoad := func(phaseName string) bool {
		now := time.Now()
		load := self.load
		switch phaseName {
		case "download":
			if load != nil {
				if !load.ended {
					// the load is already running
					return true
				}
				// one load per session: a second is refused as busy with
				// retryAfterMs 0, since no wait would admit it
				// (lag.dev/PLAN.md M3)
				retryAfterMs := int64(0)
				return self.write(&lagApiErrorMessage{Type: "error", Code: lagApiErrorBusy, RetryAfterMs: &retryAfterMs})
			}
			switch code, retryAfter := self.api.beginLoad(now, self); code {
			case "":
			case lagApiLoadReclaimed:
				// closed to make room as it asked: its close is on its way,
				// and its report says it had no load (M3, M8)
				return true
			default:
				self.refused = true
				retryAfterMs := retryAfter.Milliseconds()
				return self.write(&lagApiErrorMessage{Type: "error", Code: code, RetryAfterMs: &retryAfterMs})
			}
			load := &lagApiLoad{
				requestTime: now,
			}
			load.download = newLagApiPhase(
				self.api,
				self.ctx,
				lagApiDirectionDownload,
				self.downloadDuration,
				lagApiInterval,
				self.maxStreamCount,
				self.maxDownloadBytes,
				timings.phaseWait,
				self.wakeLoop,
			)
			self.stateLock.Lock()
			self.load = load
			self.publishWithLock()
			self.stateLock.Unlock()
			// the load's pings go back to back from now: a ping waiting out
			// the ticket's interval goes at once
			if pingC != nil {
				pingTimer.Reset(0)
			}
		case "upload":
			if load == nil || load.settled || load.upload != nil {
				return true
			}
			// the browser moves on when its download is done; whatever is
			// left of the download's window ends with it
			load.download.finish(lagApiPhaseEndClosed)
			upload := newLagApiPhase(
				self.api,
				self.ctx,
				lagApiDirectionUpload,
				self.uploadDuration,
				lagApiInterval,
				self.maxStreamCount,
				self.maxUploadBytes,
				timings.phaseWait,
				self.wakeLoop,
			)
			self.stateLock.Lock()
			load.upload = upload
			self.publishWithLock()
			self.stateLock.Unlock()
			phaseWaitTimer.Stop()
			phaseWaitC = nil
		case "done":
			if load == nil || load.ended {
				return true
			}
			if load.upload != nil {
				if firstByteTime, _, _ := load.upload.state(); !firstByteTime.IsZero() {
					// the upload moved bytes: the load settles on its own, if it
					// has not, and the result follows done once it has
					load.doneReceived = true
				}
			}
			// whatever has already run out is settled before done is read
			if !reconcile() {
				return false
			}
			switch {
			case load.ended:
				return true
			case load.settled:
				// the transfer is over and its last progress is out
				return resultLoad(load)
			case load.doneReceived:
				// the upload's window is still open, or has run out and is
				// not yet final (amendment 15): it settles on its own, and the
				// result follows at once
				return true
			}
			// done before the upload moved a byte: the browser gave the load up
			return endLoad(load, protocol.LagEndReason_LAG_END_CLIENT_CLOSED, now)
		}
		return reconcile()
	}

	for {
		ok := true
		select {
		case <-self.ctx.Done():
			return

		case <-pingC:
			pingC = nil
			payload := make([]byte, lagApiPingPayloadByteCount)
			if _, err := rand.Read(payload); err != nil {
				self.api.server.reportError("lag ping", err)
				ok = false
				break
			}
			inFlightPayload = string(payload)
			inFlightSendTime = time.Now()
			self.inFlight.Store(&lagApiInFlightPing{payload: inFlightPayload, sendTime: inFlightSendTime})
			if err := self.conn.WriteControl(websocket.PingMessage, payload, inFlightSendTime.Add(timings.writeTimeout)); err != nil {
				self.api.server.reportError("lag ping", err)
				ok = false
				break
			}
			lossTimer.Reset(timings.pingLost)
			lossC = lossTimer.C

		case pong := <-self.pongs:
			if lossC == nil || pong.appData != inFlightPayload || pong.arrivalTime.Before(inFlightSendTime) {
				// a pong for a ping already counted as lost
				break
			}
			self.inFlight.Store(nil)
			lossTimer.Stop()
			lossC = nil
			rtt := pong.arrivalTime.Sub(inFlightSendTime)
			keepPing(lagApiPing{sendTime: inFlightSendTime, rtt: rtt})
			ms := math.Round(float64(rtt)/float64(time.Millisecond)*100) / 100
			ok = self.write(&lagApiRttMessage{
				Type: "rtt",
				T:    inFlightSendTime.Sub(self.startTime).Milliseconds(),
				Ms:   &ms,
			})
			interval := self.pingInterval
			if loading() {
				interval = lagApiLoadPingInterval
			}
			nextPingTime := inFlightSendTime.Add(interval)
			if nextPingTime.Before(pong.arrivalTime) {
				nextPingTime = pong.arrivalTime
			}
			pingTimer.Reset(time.Until(nextPingTime))
			pingC = pingTimer.C

		case <-lossC:
			lossC = nil
			// kept before it stops being in flight, so its late pong always
			// finds it as one or the other
			self.addLostPing(lagApiInFlightPing{payload: inFlightPayload, sendTime: inFlightSendTime}, timings.pongTimeout)
			self.inFlight.Store(nil)
			keepPing(lagApiPing{sendTime: inFlightSendTime, lost: true})
			ok = self.write(&lagApiRttMessage{
				Type: "rtt",
				T:    inFlightSendTime.Sub(self.startTime).Milliseconds(),
				Lost: true,
			})
			if timings.pongTimeout <= time.Since(self.lastPongTime()) {
				// the page is gone: no pong that answers a ping for
				// pongTimeout ends the session with 4408, while late pongs
				// keep a slow page's session open (lag.dev/PLAN.md M4)
				self.end(lagApiCloseCodeTimeout, "timeout", protocol.LagEndReason_LAG_END_CLIENT_CLOSED)
				return
			}
			pingTimer.Reset(0)
			pingC = pingTimer.C

		case message := <-self.messages:
			switch message.Type {
			case "load":
				ok = handleLoad(message.Phase)
			case "received":
				if load := self.load; load != nil && !self.reported {
					if firstByteTime, _, _ := load.download.state(); !firstByteTime.IsZero() {
						load.received.add(message.Ms, message.Bytes, load.download.consumed(), self.downloadDuration)
					}
				}
			}

		case <-self.wake:
			ok = reconcile()

		case <-phaseWaitC:
			phaseWaitC = nil
			if load := self.load; load != nil && !load.settled && load.upload == nil {
				ok = endLoad(load, protocol.LagEndReason_LAG_END_TIMEOUT, time.Now())
			}

		case <-progressC:
			progressC = nil
			if load := self.load; load != nil && !load.settled && load.upload != nil {
				// the browser's client takes a progress within half an
				// interval of the window's length as the window closing (S5),
				// which only the last one says: a tick that late is not sent
				ok = writeUploaded(load, false)
				scheduleProgress(load)
			}

		case <-doneC:
			doneC = nil
			if load := self.load; load != nil && load.settled && !load.ended {
				ok = resultLoad(load)
			}

		case <-reportC:
			reportC = nil
			self.report(time.Now())
		}
		if !ok {
			self.end(lagApiCloseCodeInternal, lagApiErrorInternal, protocol.LagEndReason_LAG_END_ERROR)
			return
		}
		self.updateFinished()
	}
}

// Marks the session finished once its load has ended, its result with the
// browser, and its report has gone out (M3, M8): from then on it only pings,
// so a full extender may close it, first of all, to admit a new session. A
// session with no load reports only when it closes, so it never finishes;
// a full extender may close it once it has been open the idle reclaim wait
// (reclaimableSessionWithLock). Read after each step of the loop, whose state
// this is.
func (self *lagApiSession) updateFinished() {
	if !self.finished.Load() && self.reported && self.load != nil && self.load.ended {
		self.finished.Store(true)
	}
}

// The end of the report's minute for a load that ended (M8): reportAfter past
// the load's end, and never more than reportWindow from the minute's start.
// lag.dev/PLAN.md M8, A7: the minute is held to reportWindow, so a gap between
// the download and the upload does not put a ping past A7's 60 s.
func (self *lagApiSession) reportEndTime(load *lagApiLoad) time.Time {
	timings := self.api.timings
	endTime := load.endTime.Add(timings.reportAfter)
	if limitTime := self.reportStartTime(load).Add(timings.reportWindow); limitTime.Before(endTime) {
		endTime = limitTime
	}
	return endTime
}

// The start of the report's minute for a load (M8): reportBefore ahead of the
// download's first byte, or of the load's request when no byte moved.
func (self *lagApiSession) reportStartTime(load *lagApiLoad) time.Time {
	anchorTime, _, _ := load.download.state()
	if anchorTime.IsZero() {
		anchorTime = load.requestTime
	}
	return anchorTime.Add(-self.api.timings.reportBefore)
}

// Ends the session after the read side is done and the loop has returned: a
// load still running ends with the session -- as the browser closing it, or as
// the extender ended it -- and the report goes out with what it has (M8). An
// upload whose window had run out when the session ended, final or not yet
// (amendment 15), ended by its time, and the load as it would have at the
// window's end: the session's end makes the window final, with what it has.
func (self *lagApiSession) finish() {
	now := time.Now()
	if load := self.load; load != nil {
		if !load.settled {
			load.settled = true
			load.download.finish(lagApiPhaseEndClosed)
			if load.upload != nil {
				load.upload.finish(lagApiPhaseEndClosed)
			}
			load.endTime = now
			load.endReason = self.loadEndReason()
			if load.upload != nil {
				if _, uploadEndedTime, uploadEnd := load.upload.state(); uploadEnd == lagApiPhaseEndTime {
					_, _, downloadEnd := load.download.state()
					load.endTime = uploadEndedTime
					load.endReason = protocol.LagEndReason_LAG_END_COMPLETE
					if load.capped || downloadEnd == lagApiPhaseEndCap {
						load.endReason = protocol.LagEndReason_LAG_END_CAP
					}
				}
			}
			self.api.endLoad(self)
		}
		load.ended = true
	}
	self.report(now)
	self.api.removeSession(self)
}

// Builds, signs and submits the session's one report (M8), at the close of its
// minute or when the session ends first.
func (self *lagApiSession) report(now time.Time) {
	if self.reported {
		return
	}
	self.reported = true
	report, err := connect.SignLagReport(self.reportBody(now), self.api.identityPrivateKey)
	if err != nil {
		self.api.server.reportError("lag report", err)
		return
	}
	if self.api.reports != nil {
		self.api.reports.Submit(report)
	}
}

// The report's body at now (M8). With a load it is the minute around the load,
// cut at now when the session ended first; without, the session's last minute,
// ending as busy when a load was refused and as no load otherwise.
func (self *lagApiSession) reportBody(now time.Time) *protocol.LagReportBody {
	timings := self.api.timings
	body := &protocol.LagReportBody{
		TicketId:           self.ticketId,
		ExtenderPublicKey:  self.api.identityPublicKey,
		TrackId:            self.trackId,
		TrackPosition:      self.trackPosition,
		IpFamily:           self.ipFamily,
		SessionStartTimeMs: uint64(self.startTime.UnixMilli()),
		IntervalMs:         uint32(lagApiInterval.Milliseconds()),
	}
	msAfter := func(startTime time.Time, t time.Time) uint32 {
		return uint32(max(0, t.Sub(startTime).Milliseconds()))
	}

	var windowStartTime time.Time
	var windowEndTime time.Time
	if load := self.load; load != nil {
		windowStartTime = self.reportStartTime(load)
		windowEndTime = now
		if load.settled {
			windowEndTime = self.reportEndTime(load)
			if now.Before(windowEndTime) {
				windowEndTime = now
			}
		}
		body.EndReason = load.endReason
		phaseTimes := func(phase *lagApiPhase) (uint32, uint32) {
			if phase == nil {
				return 0, 0
			}
			firstByteTime, endedTime, _ := phase.state()
			if firstByteTime.IsZero() {
				return 0, 0
			}
			if endedTime.IsZero() {
				endedTime = now
			}
			return msAfter(windowStartTime, firstByteTime), msAfter(firstByteTime, endedTime)
		}
		body.DownloadStartMs, body.DownloadMs = phaseTimes(load.download)
		body.UploadStartMs, body.UploadMs = phaseTimes(load.upload)
		body.DownloadReceivedBytes = load.received.values()
		body.DownloadSentBytes = load.download.seriesValues()
		body.DownloadStreams = uint32(load.download.streamCount())
		if load.upload != nil {
			body.UploadBytes = load.upload.windowValues()
			body.UploadStreams = uint32(load.upload.streamCount())
		}
		body.UploadVerified = load.uploadVerified()
	} else {
		windowEndTime = now
		windowStartTime = now.Add(-timings.reportWindow)
		if windowStartTime.Before(self.startTime) {
			windowStartTime = self.startTime
		}
		body.EndReason = protocol.LagEndReason_LAG_END_NO_LOAD
		if self.refused {
			body.EndReason = protocol.LagEndReason_LAG_END_BUSY
		}
	}
	body.WindowStartTimeMs = uint64(windowStartTime.UnixMilli())
	for _, ping := range self.pings.between(windowStartTime, windowEndTime) {
		body.PingOffsetMs = append(body.PingOffsetMs, msAfter(windowStartTime, ping.sendTime))
		pingMicros := uint32(0)
		if !ping.lost {
			// 0 means lost, so a measured round trip is at least 1
			pingMicros = uint32(max(1, min(ping.rtt.Microseconds(), math.MaxUint32)))
		}
		body.PingMicros = append(body.PingMicros, pingMicros)
	}
	return body
}
