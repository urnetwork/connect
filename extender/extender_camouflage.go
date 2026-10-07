package extender

// extender_camouflage.go — the server half of the tcp carrier camouflage
// (EXTENDER.md P2, P3, P4, P5).
//
// The accept path gains a peek in front of tls.Server. The server reassembles
// the first handshake record or records into the complete ClientHello without
// consuming them, parses it (through utls.UnmarshalClientHello) for the random,
// the session id, the key shares, the sni and the alpn, keeps the read bytes to
// replay to whatever serves the connection next, and classifies it:
//
//   - Authenticated (tag opens, version in bounds, short id matches, time in
//     window, not a replay): tls.Server with the identity leaf and a http/1.1
//     alone config (P4). The existing v2 http request path runs inside
//     unchanged and the client verifies the leaf under the record key (B3).
//   - Unauthenticated, splice on: the hello is spliced to the real borrowed site
//     named in the sni (P3, P5) over a raw tcp relay with the A5 byte and idle
//     bounds, a total concurrent-splice cap and a per-target cap, and no O1
//     accounting. A prober — including a replayer (P2) — sees the real site.
//   - Unauthenticated, splice off: terminated as today with the per-name leaf.
//
// Replay protection, the one thing reality lacks (P2), is a bounded,
// time-windowed seen-tag set keyed on the 32-byte sealed session id: a tag
// already present is handled exactly as an unauthenticated hello, so a replay is
// indistinguishable to the prober from any other probe.

import (
	"container/list"
	"context"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"

	"crypto/ecdh"

	"github.com/urnetwork/connect"
)

// Relay buffer of one spliced direction, the reverse proxy's buffer size.
const extenderCamouflageSpliceBufferByteCount = 4096

// The tcp carrier camouflage of one extender (P). Safe for concurrent use.
type extenderCamouflage struct {
	server   *ExtenderServer
	settings *ExtenderSettings

	// the X25519 static private key derived from the identity seed (P1), nil
	// when the extender has no identity key; its public half is published
	staticPrivateKey *ecdh.PrivateKey
	staticPublicKey  []byte
	// the 8-byte short id of the identity key both ends compute (P1)
	shortId []byte

	// the verified borrowed names this extender splices to (P5), lowercased
	borrowNames []string

	replay *extenderCamouflageReplaySet

	// retirement and the live splice slots of the total and per-target caps
	// (P3); resolver construction and shutdown never hold this lock
	stateLock          sync.Mutex
	closed             bool
	spliceCount        int
	spliceTargetCounts map[string]int

	// the lazily built DoH cache the default splice dial resolves over; every
	// access follows dohOnce.Do, including retirement joining its constructor
	dohOnce  sync.Once
	dohCache *connect.DohCache
}

// Builds the camouflage of a server. An extender with no identity key gets a
// camouflage that recognizes nothing and publishes no key, so the field is
// never nil on a server with an identity but the recognition still gates on
// CamouflageEnabled. A static-key derivation failure is an error, reported the
// way a certificate error is.
func newExtenderCamouflage(server *ExtenderServer, settings *ExtenderSettings) (*extenderCamouflage, error) {
	camouflage := &extenderCamouflage{
		server:             server,
		settings:           settings,
		spliceTargetCounts: map[string]int{},
		replay:             newExtenderCamouflageReplaySet(settings),
	}
	borrowNames := []string{}
	for _, borrowName := range settings.CamouflageBorrowNames {
		if borrowName = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(borrowName), ".")); borrowName != "" {
			borrowNames = append(borrowNames, borrowName)
		}
	}
	camouflage.borrowNames = borrowNames
	if 0 < len(settings.IdentityKeySeed) {
		staticPrivateKey, err := connect.ExtenderRealityStaticPrivateKey(settings.IdentityKeySeed)
		if err != nil {
			return nil, err
		}
		publicKey, err := connect.ExtenderPublicKeyFromSeed(settings.IdentityKeySeed)
		if err != nil {
			return nil, err
		}
		camouflage.staticPrivateKey = staticPrivateKey
		camouflage.staticPublicKey = staticPrivateKey.PublicKey().Bytes()
		camouflage.shortId = connect.ExtenderKeyId(publicKey)
	}
	return camouflage, nil
}

// The published X25519 static public key, or nil without an identity key.
func (self *extenderCamouflage) StaticPublicKey() []byte {
	return slices.Clone(self.staticPublicKey)
}

// Retires splice admission before joining lazy construction and closing the
// resolver. A cold owner stays cold, and an in-flight constructor is joined.
func (self *extenderCamouflage) close() {
	self.stateLock.Lock()
	self.closed = true
	self.stateLock.Unlock()
	self.dohOnce.Do(func() {})
	if self.dohCache != nil {
		self.dohCache.Close()
	}
}

// Reports retirement without holding a splice lock during resolver or dial I/O.
func (self *extenderCamouflage) isClosed() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.closed
}

// The clock the camouflage reads (P1, P2).
func (self *extenderCamouflage) now() time.Time {
	if self.settings.CamouflageNow != nil {
		return self.settings.CamouflageNow()
	}
	return time.Now()
}

// camouflageDemultiplex peeks the raw ClientHello and classifies the tcp
// connection (P3). It returns the connection the rest of the handler should
// terminate with the peeked bytes replayed, whether that termination is the
// authenticated path (http/1.1 alone, P4), and whether the connection was
// already handled (a splice). When camouflage is off it is a no-op returning the
// connection unchanged.
func (self *ExtenderServer) camouflageDemultiplex(ctx context.Context, conn net.Conn) (net.Conn, bool, bool) {
	camouflage := self.camouflage
	if camouflage == nil || !self.settings.CamouflageEnabled || camouflage.staticPrivateKey == nil {
		return conn, false, false
	}

	// the peek reads under the same budget the handshake has (A9); tls.Server
	// resets the deadline for the handshake and the request that follow
	conn.SetDeadline(time.Now().Add(self.settings.HeaderTimeout))
	peekCanceled := make(chan struct{})
	stopPeekCancel := context.AfterFunc(ctx, func() {
		defer close(peekCanceled)
		conn.Close()
	})

	maxByteCount := self.settings.ExtenderCamouflageHelloMaxByteCount
	if maxByteCount <= 0 {
		maxByteCount = DefaultExtenderSettings().ExtenderCamouflageHelloMaxByteCount
	}
	readBytes, parsed, ok := peekClientHello(conn, maxByteCount)
	// Join a running cancellation before handing the connection to tls or the
	// splice, so no late callback can interfere with the next phase's deadline.
	if !stopPeekCancel() {
		<-peekCanceled
	}
	if ctx.Err() != nil {
		return nil, false, true
	}
	if !ok {
		// not a parseable ClientHello: terminate it as today, the peeked bytes
		// replayed so tls.Server sees whatever the peer sent
		self.camouflageTerminatedCount.Add(1)
		return newConnWithInitialBytes(conn, readBytes, ""), false, false
	}

	if camouflage.authenticate(parsed) {
		self.camouflageAuthenticatedCount.Add(1)
		camouflage.reportOutcome(parsed, ExtenderCamouflageOutcomeAuthenticated)
		return newConnWithInitialBytes(conn, readBytes, ""), true, false
	}

	// unauthenticated: splice to the real borrowed site when splice mode is on
	// and a target is reachable (P3), else terminate as today. splice counts the
	// connection when it begins, since the relay then blocks until it ends
	if self.settings.ExtenderCamouflageSplice {
		if camouflage.splice(ctx, conn, readBytes, parsed.ServerName) {
			camouflage.reportOutcome(parsed, ExtenderCamouflageOutcomeSpliced)
			return nil, false, true
		}
	}
	self.camouflageTerminatedCount.Add(1)
	camouflage.reportOutcome(parsed, ExtenderCamouflageOutcomeTerminated)
	return newConnWithInitialBytes(conn, readBytes, ""), false, false
}

// Reports one classification to the observation seam, if installed (P3).
func (self *extenderCamouflage) reportOutcome(parsed *utls.PubClientHelloMsg, outcome string) {
	if self.settings.CamouflageHandler == nil {
		return
	}
	self.settings.CamouflageHandler(ExtenderCamouflageOutcome{
		ServerName:    parsed.ServerName,
		AlpnProtocols: slices.Clone(parsed.AlpnProtocols),
		Outcome:       outcome,
	})
}

// authenticate opens the session-id tag of a parsed hello and applies the
// version, short-id, time-window and replay checks (P1, P2). The expensive ECDH
// is spent only on a hello that could be authenticated — a 32-byte session id
// and an X25519 or X25519MLKEM768 key share — so a plain Go client, a legacy
// extender client and most probers never reach it. A tag whose time is outside
// the window or whose session id is a replay increments its counter and is not
// authenticated, so the caller splices or terminates it.
func (self *extenderCamouflage) authenticate(parsed *utls.PubClientHelloMsg) bool {
	if parsed == nil || len(parsed.SessionId) != 32 || len(parsed.Random) != 32 {
		return false
	}
	group, data, ok := self.preferredEphemeral(parsed)
	if !ok {
		return false
	}
	plaintext, ok := connect.ExtenderRealityOpenSessionId(
		self.staticPrivateKey,
		parsed.Raw,
		parsed.Random,
		parsed.SessionId,
		group,
		data,
	)
	if !ok {
		return false
	}
	if !connect.ExtenderRealitySessionIdAuthorized(plaintext, self.shortId) {
		return false
	}
	now := self.now()
	if !connect.ExtenderRealitySessionIdInWindow(plaintext, now, self.timeWindow()) {
		self.server.camouflageTimeWindowFailedCount.Add(1)
		return false
	}
	// the tag opened and is in window; a session id already seen is a replay,
	// handled exactly as an unauthenticated hello (P2)
	if self.replay.seen(string(parsed.SessionId), now) {
		self.server.camouflageReplayRefusedCount.Add(1)
		return false
	}
	return true
}

// The tolerated clock skew each way (P1).
func (self *extenderCamouflage) timeWindow() time.Duration {
	if 0 < self.settings.ExtenderCamouflageTimeWindow {
		return self.settings.ExtenderCamouflageTimeWindow
	}
	return DefaultExtenderSettings().ExtenderCamouflageTimeWindow
}

// The client ephemeral key share the extender reads for the ECDH (P1): the
// standalone X25519 (group 29) when the hello offers it, else the
// X25519MLKEM768 hybrid, which is the preference the client's own seal uses, so
// both ends agree on one ephemeral without a second ECDH.
func (self *extenderCamouflage) preferredEphemeral(parsed *utls.PubClientHelloMsg) (uint16, []byte, bool) {
	var hybridGroup uint16
	var hybridData []byte
	for _, keyShare := range parsed.KeyShares {
		switch uint16(keyShare.Group) {
		case connect.ExtenderRealityGroupX25519:
			return uint16(keyShare.Group), keyShare.Data, true
		case connect.ExtenderRealityGroupX25519Mlkem768:
			hybridGroup = uint16(keyShare.Group)
			hybridData = keyShare.Data
		}
	}
	if hybridData != nil {
		return hybridGroup, hybridData, true
	}
	return 0, nil, false
}

// splice relays an unauthenticated hello to the real borrowed site (P3). It
// returns true when the connection was spliced and false when none of the
// candidate borrowed sites was reachable or a cap refused the splice, in which
// case the caller terminates the connection instead. A spliced byte is never
// added to the O1 relay counters (P10).
func (self *extenderCamouflage) splice(ctx context.Context, clientConn net.Conn, initialBytes []byte, serverName string) bool {
	targets := self.spliceTargets(serverName)
	if len(targets) == 0 {
		return false
	}
	network := forwardNetwork(clientConn.RemoteAddr().String())
	for _, target := range targets {
		// the per-target and total caps are reserved before the dial, so a
		// refusal never opens a connection to the site (P3)
		if !self.beginSplice(target) {
			continue
		}
		siteConn, err := self.spliceDial(ctx, network, target)
		if err != nil || siteConn == nil {
			if siteConn != nil {
				siteConn.Close()
			}
			self.endSplice(target)
			self.server.camouflageSpliceDialFailedCount.Add(1)
			// a borrowed site the extender cannot reach falls to another
			// reachable borrowed target of the same family (P3)
			continue
		}
		// counted when the splice begins, since relaySplice then blocks until the
		// connection ends; a spliced byte is never added to the O1 relay counts
		self.server.camouflageSplicedCount.Add(1)
		self.relaySplice(ctx, clientConn, siteConn, initialBytes)
		self.endSplice(target)
		return true
	}
	return false
}

// The borrowed sites to try, in order (P3): the sni first when it is a verified
// borrowed name, then the other verified names, so a reachable real site is
// found even when the sni's own site is down. An sni that is not a verified
// borrowed name still falls to the verified names, so a prober with an arbitrary
// sni is spliced to a real modern-tls site rather than shown the self-signed
// tell.
func (self *extenderCamouflage) spliceTargets(serverName string) []string {
	serverName = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(serverName), "."))
	targets := make([]string, 0, len(self.borrowNames))
	if serverName != "" && slices.Contains(self.borrowNames, serverName) {
		targets = append(targets, serverName)
	}
	for _, borrowName := range self.borrowNames {
		if borrowName != serverName {
			targets = append(targets, borrowName)
		}
	}
	return targets
}

// Reserves a total and a per-target splice slot (P3). False when either cap is
// full, which refuses the splice to that target.
func (self *extenderCamouflage) beginSplice(target string) bool {
	settings := self.settings
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return false
	}
	if 0 < settings.ExtenderCamouflageSpliceMaxCount && settings.ExtenderCamouflageSpliceMaxCount <= self.spliceCount {
		return false
	}
	if 0 < settings.ExtenderCamouflageSpliceMaxPerTarget && settings.ExtenderCamouflageSpliceMaxPerTarget <= self.spliceTargetCounts[target] {
		return false
	}
	self.spliceCount += 1
	self.spliceTargetCounts[target] += 1
	return true
}

// Releases a splice slot.
func (self *extenderCamouflage) endSplice(target string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.spliceCount -= 1
	if count := self.spliceTargetCounts[target] - 1; 0 < count {
		self.spliceTargetCounts[target] = count
	} else {
		delete(self.spliceTargetCounts, target)
	}
}

// Opens the tcp connection to a borrowed site for the splice (P3). The injected
// seam takes precedence; the default resolves the name over a DoH cache on the
// client's family and dials the result over the forward egress, falling back to
// an egress dial by name.
func (self *extenderCamouflage) spliceDial(ctx context.Context, network string, target string) (net.Conn, error) {
	if self.isClosed() {
		return nil, net.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if self.settings.CamouflageSpliceDialContext != nil {
		return self.settings.CamouflageSpliceDialContext(ctx, network, net.JoinHostPort(target, "443"))
	}
	recordType := "A"
	if network == "tcp6" {
		recordType = "AAAA"
	}
	dohCache := self.spliceDohCache()
	if dohCache == nil {
		return nil, net.ErrClosed
	}
	addrs := dohCache.Query(ctx, recordType, target)
	if self.isClosed() {
		return nil, net.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		if conn, err := self.server.dialContext()(ctx, network, net.JoinHostPort(addr.String(), "443")); err == nil {
			return conn, nil
		}
	}
	if self.isClosed() {
		return nil, net.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// the egress resolver, so a borrowed name the DoH cache could not resolve
	// still reaches its site over the family-narrowed egress
	return self.server.dialContext()(ctx, network, net.JoinHostPort(target, "443"))
}

// The DoH cache the default splice dial resolves over, built once from the
// server's DoH settings. Retirement may consume the once before first use;
// callers must treat nil as a closed owner, never as a missing initialization.
func (self *extenderCamouflage) spliceDohCache() *connect.DohCache {
	self.dohOnce.Do(func() {
		if self.isClosed() {
			return
		}
		dohSettings := self.settings.DohSettings
		if dohSettings == nil {
			dohSettings = connect.DefaultDohSettings()
		}
		self.dohCache = connect.NewDohCache(dohSettings)
	})
	if self.isClosed() {
		return nil
	}
	return self.dohCache
}

// relaySplice copies the spliced bytes both ways until either side ends or a
// bound is hit (P3): the peeked ClientHello is written to the site first, then
// each direction is copied with the A5 per-connection byte bound and idle
// timeout. Owns and joins cancellation from before the first write, including
// failed and partial hello writes. None of it touches the O1 relay counters (P10).
func (self *extenderCamouflage) relaySplice(ctx context.Context, clientConn net.Conn, siteConn net.Conn, initialBytes []byte) {
	idle := self.settings.ProxyIdleTimeout
	maxByteCount := self.settings.ProxyMaxResponseByteCount

	relayCtx, relayCancel := context.WithCancel(ctx)
	var relayWorkers sync.WaitGroup
	connectionsClosed := make(chan struct{})
	context.AfterFunc(relayCtx, func() {
		defer close(connectionsClosed)
		clientConn.Close()
		siteConn.Close()
	})
	defer func() {
		relayCancel()
		<-connectionsClosed
		relayWorkers.Wait()
	}()
	// The peek's header budget ends at this handoff. Relay I/O installs its
	// own idle deadlines, including leaving them unset when idle is disabled.
	clientConn.SetDeadline(time.Time{})
	writeAll := func(dst net.Conn, writeBytes []byte) bool {
		for 0 < len(writeBytes) {
			if relayCtx.Err() != nil {
				return false
			}
			if 0 < idle {
				dst.SetWriteDeadline(time.Now().Add(idle))
			}
			n, err := dst.Write(writeBytes)
			if err != nil || n <= 0 {
				return false
			}
			writeBytes = writeBytes[n:]
		}
		return true
	}
	if !writeAll(siteConn, initialBytes) {
		return
	}

	var relayedByteCount atomicInt64
	copyDirection := func(dst net.Conn, src net.Conn) {
		defer relayWorkers.Done()
		defer relayCancel()
		buffer := make([]byte, extenderCamouflageSpliceBufferByteCount)
		for {
			select {
			case <-relayCtx.Done():
				return
			default:
			}
			if 0 < idle {
				src.SetReadDeadline(time.Now().Add(idle))
			}
			n, err := src.Read(buffer)
			if 0 < n {
				if 0 < maxByteCount && maxByteCount < relayedByteCount.add(int64(n)) {
					// past the per-connection relayed-bytes bound (P3): cut the
					// splice, never counting the byte as O1 relay traffic
					return
				}
				if !writeAll(dst, buffer[:n]) {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	relayWorkers.Add(2)
	go connect.HandleError(func() { copyDirection(siteConn, clientConn) }, relayCancel)
	go connect.HandleError(func() { copyDirection(clientConn, siteConn) }, relayCancel)
	<-relayCtx.Done()
}

// A small atomic int64 so the splice relay sums its two directions without a
// lock, like the server's O1 counters, without reaching for them.
type atomicInt64 struct {
	stateLock sync.Mutex
	value     int64
}

func (self *atomicInt64) add(delta int64) int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.value += delta
	return self.value
}

// peekClientHello reassembles the first tls handshake record or records into the
// complete ClientHello without consuming them (P3). It returns every byte read
// from the socket, to replay to whatever serves the connection next, the parsed
// hello, and whether a ClientHello was parsed. A peer whose first record is not
// a handshake, or whose first handshake message is not a ClientHello, or whose
// hello does not complete within maxByteCount, is not a parseable ClientHello —
// ok false — and its read bytes are still returned so the caller can replay
// them.
func peekClientHello(conn net.Conn, maxByteCount int) ([]byte, *utls.PubClientHelloMsg, bool) {
	readBytes := []byte{}
	handshakeBytes := []byte{}
	for {
		recordHeader := make([]byte, 5)
		n, err := io.ReadFull(conn, recordHeader)
		readBytes = append(readBytes, recordHeader[:n]...)
		if err != nil {
			return readBytes, nil, false
		}
		// tls record: content type 22 is handshake; anything else is not a
		// ClientHello flight
		if recordHeader[0] != 22 {
			return readBytes, nil, false
		}
		recordByteCount := int(recordHeader[3])<<8 | int(recordHeader[4])
		if recordByteCount <= 0 || maxByteCount < len(handshakeBytes)+recordByteCount {
			return readBytes, nil, false
		}
		fragment := make([]byte, recordByteCount)
		n, err = io.ReadFull(conn, fragment)
		readBytes = append(readBytes, fragment[:n]...)
		if err != nil {
			return readBytes, nil, false
		}
		handshakeBytes = append(handshakeBytes, fragment...)

		if len(handshakeBytes) < 4 {
			continue
		}
		// handshake message: type 1 is ClientHello, then a uint24 length
		if handshakeBytes[0] != 1 {
			return readBytes, nil, false
		}
		messageByteCount := int(handshakeBytes[1])<<16 | int(handshakeBytes[2])<<8 | int(handshakeBytes[3])
		if maxByteCount < 4+messageByteCount {
			return readBytes, nil, false
		}
		if len(handshakeBytes) < 4+messageByteCount {
			continue
		}
		parsed := utls.UnmarshalClientHello(handshakeBytes[:4+messageByteCount])
		if parsed == nil {
			return readBytes, nil, false
		}
		return readBytes, parsed, true
	}
}

// The bounded, time-windowed seen-tag set that refuses replays (P2). Keyed on
// the 32-byte sealed session id, which is a deterministic function of the
// client's random, so two genuine dials never collide. Entries expire after
// twice the time window and the set is capped, oldest evicted. Safe for
// concurrent use.
type extenderCamouflageReplaySet struct {
	settings *ExtenderSettings

	stateLock sync.Mutex
	// session id to its order element, newest at the front
	entries map[string]*list.Element
	order   *list.List
}

// One seen tag: the session id and when it was inserted, so the oldest can be
// found for expiry and eviction.
type extenderCamouflageReplayEntry struct {
	sessionId  string
	insertTime time.Time
}

func newExtenderCamouflageReplaySet(settings *ExtenderSettings) *extenderCamouflageReplaySet {
	return &extenderCamouflageReplaySet{
		settings: settings,
		entries:  map[string]*list.Element{},
		order:    list.New(),
	}
}

// seen reports whether sessionId was already in the set, and inserts it when it
// was not (P2). A true answer is a replay. Expired entries are pruned and the
// set is held under its cap on every call, so it never grows past the bound.
func (self *extenderCamouflageReplaySet) seen(sessionId string, now time.Time) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	// an entry older than twice the time window would fail the time check
	// anyway, so nothing older need be remembered (P2)
	expiry := 2 * self.timeWindow()
	for {
		oldest := self.order.Back()
		if oldest == nil {
			break
		}
		entry := oldest.Value.(*extenderCamouflageReplayEntry)
		if now.Sub(entry.insertTime) < expiry {
			break
		}
		self.order.Remove(oldest)
		delete(self.entries, entry.sessionId)
	}

	if _, ok := self.entries[sessionId]; ok {
		return true
	}

	element := self.order.PushFront(&extenderCamouflageReplayEntry{
		sessionId:  sessionId,
		insertTime: now,
	})
	self.entries[sessionId] = element
	for maxCount := self.maxCount(); 0 < maxCount && maxCount < self.order.Len(); {
		oldest := self.order.Back()
		if oldest == nil {
			break
		}
		entry := oldest.Value.(*extenderCamouflageReplayEntry)
		self.order.Remove(oldest)
		delete(self.entries, entry.sessionId)
	}
	return false
}

func (self *extenderCamouflageReplaySet) timeWindow() time.Duration {
	if 0 < self.settings.ExtenderCamouflageTimeWindow {
		return self.settings.ExtenderCamouflageTimeWindow
	}
	return DefaultExtenderSettings().ExtenderCamouflageTimeWindow
}

func (self *extenderCamouflageReplaySet) maxCount() int {
	if 0 < self.settings.ExtenderCamouflageReplayTagCount {
		return self.settings.ExtenderCamouflageReplayTagCount
	}
	return DefaultExtenderSettings().ExtenderCamouflageReplayTagCount
}
