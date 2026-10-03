package extender

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	mathrand "math/rand"
	"net"
	"slices"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// NLayer extenders (EXTENDER.md A11).
//
// An NLayer extender does not forward a request to its destination. It relays
// the inner stream to one of a preset list of other extenders, its hops, as an
// extender client of that hop (a probe is relayed the same way, to the end of
// the chain: extender_nlayer_probe.go): a fresh outer tls on the hop's own carrier, a
// new header with the same destination and datagram mode one layer deeper, the
// hop's response, and then the inner bytes both ways, untouched. The client's
// inner tls to the destination stays opaque at every layer and nothing is
// reframed: the datagram framing of a datagram request passes through to the
// last layer, which is the extender that turns it into udp.
//
// Hops are balanced at random among those not held, preferring the family of
// the client's outer socket. A hop that cannot be dialed is held for
// NLayerHoldTimeout and the connection tries another, up to NLayerAttempts. A
// hop that answers with a refusal is not held: a refusal can be about the
// request, its destination or its depth, rather than about the hop. Nor is a
// hop whose dial this host's memory budget refused, which is this host's
// condition; the connection is refused instead.
//
// Two guards end a chain that loops. The depth bound refuses a forward whose
// HopCount says this extender would be deeper than NLayerMaxDepth, on every
// extender. The loop check, on an NLayer extender, refuses a stream whose inner
// tls client random this extender is already relaying on another connection:
// the request has come back to an extender it already crossed. The check reads
// only the start of the first inner record, so a stream that does not begin
// with a ClientHello -- a datagram request, or a plain inner stream -- is left
// to the depth bound.
//
// The hold state, the counts of NLayerStats and the in-flight client randoms
// are guarded by the server's stateLock, and everything here is safe for
// concurrent use.

// The start of a tls record that reaches the end of a ClientHello's random
// (RFC 8446 5.1, 4, 4.1.2): the record header, the handshake header, the
// legacy version, then the 32 random bytes. These are wire facts, not
// tunables.
const (
	tlsRecordHeaderByteCount    = 5
	tlsHandshakeHeaderByteCount = 4
	tlsLegacyVersionByteCount   = 2
	tlsClientRandomByteCount    = 32
	// a plaintext record carries at most 2^14 bytes
	tlsMaxPlaintextByteCount = 16 * 1024

	tlsContentTypeHandshake       = 22
	tlsHandshakeTypeClientHello   = 1
	tlsVersionMajor               = 3
	tlsClientRandomOffset         = tlsRecordHeaderByteCount + tlsHandshakeHeaderByteCount + tlsLegacyVersionByteCount
	tlsClientHelloPrefixByteCount = tlsClientRandomOffset + tlsClientRandomByteCount
)

// One NLayer hop: its configuration, copied at construction and never changed,
// and what this server has seen of it, which stateLock guards.
type extenderNLayerHop struct {
	// nil for a nil entry of NLayerHops, which is never picked; the entry is
	// kept so every index still names the hop it was configured as
	extenderConfig *connect.ExtenderConfig
	// 4 or 6, the family of the hop's address
	ipVersion int

	// the hop is skipped until then
	heldUntil time.Time
	// the hold has been reported and its release has not
	held bool
	// the hop answered 429 and is skipped until then, which is not a hold
	// (A12)
	limitedUntil time.Time

	relayCount   int64
	refusedCount int64
	failedCount  int64
	limitedCount int64
}

// What an NLayer extender has seen of one hop, at the hop's index in
// NLayerHops (A11). The counts are cumulative for the life of
// the server.
type ExtenderNLayerHopStats struct {
	// connections relayed through the hop
	RelayCount int64
	// dials the hop answered with a refusal, which do not hold it
	RefusedCount int64
	// dials that did not reach the hop, each of which holds it
	FailedCount int64
	// the hop is skipped until then; zero when it is not held
	HeldUntil time.Time
	// dials the hop answered 429 (A12), each of which limits it without a
	// hold
	LimitedCount int64
	// the other hops are preferred until then; zero when it is not limited
	LimitedUntil time.Time
}

// Copies the configured hops, so a caller that changes its settings after
// construction cannot race a dial.
func newExtenderNLayerHops(extenderConfigs []*connect.ExtenderConfig) []*extenderNLayerHop {
	hops := make([]*extenderNLayerHop, 0, len(extenderConfigs))
	for _, extenderConfig := range extenderConfigs {
		hop := &extenderNLayerHop{}
		if extenderConfig != nil {
			hopConfig := *extenderConfig
			hopConfig.PublicKey = slices.Clone(extenderConfig.PublicKey)
			hop.extenderConfig = &hopConfig
			hop.ipVersion = 6
			if hopConfig.Ip.Unmap().Is4() {
				hop.ipVersion = 4
			}
		}
		hops = append(hops, hop)
	}
	return hops
}

// The connect settings of every hop dial: the connect defaults, bounded by
// NLayerDialTimeout, over this extender's own egress seam, so a hop dial leaves
// the host the way a forward does and a test that injects the seam observes
// both. Built once, since the defaults load the platform roots.
func (self *ExtenderServer) newNLayerConnectSettings() *connect.ConnectSettings {
	connectSettings := connect.DefaultConnectSettings()
	connectSettings.DialContextSettings = &connect.DialContextSettings{
		// resolved per dial, exactly as the forward resolves it
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return self.dialContext()(ctx, network, address)
		},
	}
	if dialTimeout := self.settings.NLayerDialTimeout; 0 < dialTimeout {
		connectSettings.ConnectTimeout = min(connectSettings.ConnectTimeout, dialTimeout)
		connectSettings.TlsTimeout = min(connectSettings.TlsTimeout, dialTimeout)
		connectSettings.HandshakeTimeout = min(connectSettings.HandshakeTimeout, dialTimeout)
		connectSettings.RequestTimeout = min(connectSettings.RequestTimeout, dialTimeout)
	}
	return connectSettings
}

// The depth bound (A11). The header's HopCount is how many extenders the
// request has already crossed, so this extender is number HopCount + 1 of its
// chain, and a chain holds at most NLayerMaxDepth. A count that cannot be made
// one deeper is refused whatever the bound, so a hop can never wrap it.
func (self *ExtenderServer) checkNLayerDepth(header *protocol.ExtenderHeader) error {
	depth := int64(header.HopCount) + 1
	if maxDepth := self.settings.NLayerMaxDepth; 0 < maxDepth && int64(maxDepth) < depth {
		return fmt.Errorf("the request would be %d extenders deep, at most %d", depth, maxDepth)
	}
	if header.HopCount == ^uint32(0) {
		return fmt.Errorf("the request has crossed %d extenders", header.HopCount)
	}
	return nil
}

// Serves one accepted forward request of an NLayer extender, after its
// response: the loop check on a stream, the hop dial, then the plain relay for
// a stream and a datagram request alike, since the datagram frames are the
// last layer's to read (A11).
func (self *ExtenderServer) serveNLayer(
	ctx context.Context,
	cancel context.CancelFunc,
	clientConn net.Conn,
	clientAddress string,
	header *protocol.ExtenderHeader,
) {
	if !header.Datagram && 0 < self.settings.NLayerClientHelloTimeout {
		prefixBytes, clientRandom, ok := self.peekNLayerClientRandom(clientConn)
		if ok {
			if !self.beginNLayerClientRandom(clientRandom) {
				self.reportError("nlayer loop", fmt.Errorf(
					"the inner client random is already relayed by this extender",
				))
				return
			}
			defer self.endNLayerClientRandom(clientRandom)
		}
		if 0 < len(prefixBytes) {
			// the bytes the check read are the first the hop must see
			clientConn = newConnWithInitialBytes(clientConn, prefixBytes, "")
		}
	}

	hopConn, _, err := self.dialNLayerHop(ctx, clientAddress, &connect.ExtenderDial{
		DestinationHost: header.DestinationHost,
		DestinationPort: int(header.DestinationPort),
		Service:         connect.ExtenderServiceForward,
		Datagram:        header.Datagram,
		HopCount:        header.HopCount + 1,
	})
	if err != nil {
		return
	}
	defer hopConn.Close()

	self.relay(ctx, cancel, clientConn, hopConn)
}

// Reads the start of an inner stream, at most through the random of a
// ClientHello, and returns the bytes read and the random when the stream starts
// with one. Reading stops at the first byte that rules a ClientHello out, at
// NLayerClientHelloTimeout or at an error, so a stream that is not tls costs
// at most that wait, and the caller relays the bytes read ahead of the rest.
// Only the first record of a connection is looked at, so a HelloRetryRequest,
// whose second ClientHello repeats the random on the same connection, is never
// mistaken for a loop.
//
// A read that times out leaves the stream as it was on every carrier: tls keeps
// a partial record for the next read, and on the udp carriers the deadline
// falls before the first inner DATA frame, whose header arrives with its
// payload rather than split across the timeout.
func (self *ExtenderServer) peekNLayerClientRandom(clientConn net.Conn) ([]byte, [32]byte, bool) {
	prefixBytes := make([]byte, tlsClientHelloPrefixByteCount)
	if err := clientConn.SetReadDeadline(time.Now().Add(self.settings.NLayerClientHelloTimeout)); err != nil {
		return nil, [32]byte{}, false
	}
	n := 0
	for n < len(prefixBytes) && isTlsClientHelloPrefix(prefixBytes[:n]) {
		m, err := clientConn.Read(prefixBytes[n:])
		n += m
		if err != nil {
			// a timeout leaves the stream usable, since the relay sets its own
			// deadlines; any other error the relay meets on its first read
			break
		}
	}
	clientConn.SetReadDeadline(time.Time{})
	clientRandom, ok := tlsClientHelloRandom(prefixBytes[:n])
	return prefixBytes[:n], clientRandom, ok
}

// Reports whether b can still be the start of a tls record carrying a
// ClientHello, judging only the bytes it has (RFC 8446 5.1, 4, 4.1.2): a
// handshake record of a 3.x record version whose length can hold the random,
// then a client_hello whose length can too, then a 3.x legacy version. The
// empty prefix can.
func isTlsClientHelloPrefix(b []byte) bool {
	if 1 <= len(b) && b[0] != tlsContentTypeHandshake {
		return false
	}
	if 2 <= len(b) && b[1] != tlsVersionMajor {
		return false
	}
	if tlsRecordHeaderByteCount <= len(b) {
		recordByteCount := int(binary.BigEndian.Uint16(b[3:5]))
		// the random must lie inside the first record for this record alone
		// to name it
		minRecordByteCount := tlsClientHelloPrefixByteCount - tlsRecordHeaderByteCount
		if recordByteCount < minRecordByteCount || tlsMaxPlaintextByteCount < recordByteCount {
			return false
		}
	}
	if 6 <= len(b) && b[5] != tlsHandshakeTypeClientHello {
		return false
	}
	if tlsRecordHeaderByteCount+tlsHandshakeHeaderByteCount <= len(b) {
		// a 24 bit length, which may run past this record when the hello is
		// split over several
		helloByteCount := int(b[6])<<16 | int(b[7])<<8 | int(b[8])
		if helloByteCount < tlsLegacyVersionByteCount+tlsClientRandomByteCount {
			return false
		}
	}
	if 10 <= len(b) && b[9] != tlsVersionMajor {
		return false
	}
	return true
}

// The client random of the ClientHello whose record starts b, when b holds it
// whole.
func tlsClientHelloRandom(b []byte) ([32]byte, bool) {
	var clientRandom [32]byte
	if len(b) < tlsClientHelloPrefixByteCount || !isTlsClientHelloPrefix(b) {
		return clientRandom, false
	}
	copy(clientRandom[:], b[tlsClientRandomOffset:tlsClientHelloPrefixByteCount])
	return clientRandom, true
}

// Admits one inner client random to the in-flight set for the life of a
// relay, refusing a random another connection of this extender already holds.
func (self *ExtenderServer) beginNLayerClientRandom(clientRandom [32]byte) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if 0 < self.nlayerClientRandomCounts[clientRandom] {
		return false
	}
	self.nlayerClientRandomCounts[clientRandom] += 1
	return true
}

// Releases a random when its relay ends.
func (self *ExtenderServer) endNLayerClientRandom(clientRandom [32]byte) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if count := self.nlayerClientRandomCounts[clientRandom] - 1; 0 < count {
		self.nlayerClientRandomCounts[clientRandom] = count
	} else {
		delete(self.nlayerClientRandomCounts, clientRandom)
	}
}

// Dials one NLayer hop with the request a layer sends onward -- a forward to
// the same destination in the same datagram mode, or a probe for the same
// pinger (GEOMAP §2.9), either one layer deeper -- and returns the hop's
// stream, positioned after its response, and the response (A11). A hop that
// cannot be reached is held and another is tried, up to NLayerAttempts; a hop
// that refuses is counted and not held; and a claim this host's memory budget
// refuses ends the connection with the hop untouched.
func (self *ExtenderServer) dialNLayerHop(
	ctx context.Context,
	clientAddress string,
	extenderDial *connect.ExtenderDial,
) (net.Conn, *protocol.ExtenderResponse, error) {
	clientIpVersion := 0
	switch forwardNetwork(clientAddress) {
	case "tcp4":
		clientIpVersion = 4
	case "tcp6":
		clientIpVersion = 6
	}
	attemptCount := max(1, self.settings.NLayerAttempts)
	triedIndexes := []int{}
	var lastErr error
	for len(triedIndexes) < attemptCount {
		index, ok := self.pickNLayerHop(clientIpVersion, triedIndexes)
		if !ok {
			break
		}
		triedIndexes = append(triedIndexes, index)

		hopConn, hopResponse, err := self.dialNLayerHopIndex(ctx, index, extenderDial)
		if err == nil {
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.nlayerHops[index].relayCount += 1
			}()
			return hopConn, hopResponse, nil
		}
		lastErr = fmt.Errorf("NLayer hop %d: %w", index, err)
		self.reportError("nlayer hop dial", lastErr)

		var refusedErr *connect.ExtenderRefusedError
		var limitedErr *connect.ExtenderLimitedError
		switch {
		case errors.As(err, &limitedErr):
			// the hop is over its admission limits (A12): not a failure, so
			// it is limited rather than held, and another hop is tried
			self.limitNLayerHop(index, limitedErr.RetryAfter)
		case errors.As(err, &refusedErr):
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.nlayerHops[index].refusedCount += 1
			}()
		case ctx.Err() != nil:
			// the client or the server went away mid dial, which says nothing
			// about the hop
			return nil, nil, lastErr
		case connect.IsExtenderMemoryBudgetError(err):
			// this host has no memory for another carrier: a local condition,
			// which leaves the hop live and which every other hop would meet
			// the same way, so the connection is refused here
			return nil, nil, lastErr
		default:
			self.holdNLayerHop(index, err)
		}
	}
	if retryAfter, limited := self.nlayerHopsLimited(); limited {
		// every hop left is limited, which the client is told as a limit,
		// with the shortest backoff, and not as a refusal (A12)
		limitedErr := &connect.ExtenderLimitedError{RetryAfter: retryAfter}
		self.reportError("nlayer hop dial", limitedErr)
		return nil, nil, limitedErr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no NLayer hop is available")
		self.reportError("nlayer hop dial", lastErr)
	}
	return nil, nil, lastErr
}

// Limits a hop that answered 429 for the Retry-After it gave, or
// NLayerLimitedBackoff when it gave none, jittered as a client jitters it
// (A12). A later backoff is never shortened by an earlier one.
func (self *ExtenderServer) limitNLayerHop(index int, retryAfter time.Duration) {
	limitedUntil := time.Now().Add(connect.JitterExtenderLimitedBackoff(retryAfter, self.settings.NLayerLimitedBackoff))
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	hop := self.nlayerHops[index]
	hop.limitedCount += 1
	if hop.limitedUntil.Before(limitedUntil) {
		hop.limitedUntil = limitedUntil
	}
}

// Whether no hop can be dialed now because the ones not held are all limited,
// and if so the shortest remaining backoff among them (A12). An extender with
// no hop, or with one hop that is only held, is not limited.
func (self *ExtenderServer) nlayerHopsLimited() (time.Duration, bool) {
	now := time.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	var shortest time.Duration
	limited := false
	for _, hop := range self.nlayerHops {
		if hop.extenderConfig == nil || (hop.held && now.Before(hop.heldUntil)) {
			continue
		}
		remaining := hop.limitedUntil.Sub(now)
		if remaining <= 0 {
			// a hop that can be dialed now
			return 0, false
		}
		if !limited || remaining < shortest {
			shortest = remaining
		}
		limited = true
	}
	return shortest, limited
}

// Picks a hop for one connection at random among those not held and not yet
// tried by it, preferring the family of the client's outer socket so a chain
// egresses on the family its client reached it on (A7). A hop whose hold has
// run out is released here, which is when its release is reported.
func (self *ExtenderServer) pickNLayerHop(clientIpVersion int, triedIndexes []int) (int, bool) {
	now := time.Now()
	releasedIndexes := []int{}
	familyIndexes := []int{}
	otherIndexes := []int{}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for index, hop := range self.nlayerHops {
			if hop.extenderConfig == nil {
				continue
			}
			if hop.held {
				if now.Before(hop.heldUntil) {
					continue
				}
				hop.held = false
				releasedIndexes = append(releasedIndexes, index)
			}
			if slices.Contains(triedIndexes, index) {
				continue
			}
			if now.Before(hop.limitedUntil) {
				// the other hops first, until the limit passes (A12)
				continue
			}
			if clientIpVersion == 0 || hop.ipVersion == clientIpVersion {
				familyIndexes = append(familyIndexes, index)
			} else {
				otherIndexes = append(otherIndexes, index)
			}
		}
	}()
	for _, index := range releasedIndexes {
		if self.settings.NLayerHoldHandler != nil {
			self.settings.NLayerHoldHandler(index, false, nil)
		}
	}

	candidateIndexes := familyIndexes
	if len(candidateIndexes) == 0 {
		candidateIndexes = otherIndexes
	}
	if len(candidateIndexes) == 0 {
		return 0, false
	}
	return candidateIndexes[mathrand.Intn(len(candidateIndexes))], true
}

// One dial of one hop, bounded by NLayerDialTimeout. The dial context bounds
// only the establishment: the stream it returns outlives it.
func (self *ExtenderServer) dialNLayerHopIndex(
	ctx context.Context,
	index int,
	extenderDial *connect.ExtenderDial,
) (net.Conn, *protocol.ExtenderResponse, error) {
	dialCtx := ctx
	if dialTimeout := self.settings.NLayerDialTimeout; 0 < dialTimeout {
		var dialCancel context.CancelFunc
		dialCtx, dialCancel = context.WithTimeout(ctx, dialTimeout)
		defer dialCancel()
	}
	hopConn, hopResponse, err := connect.DialExtender(
		dialCtx,
		self.nlayerConnectSettings,
		self.nlayerHops[index].extenderConfig,
		extenderDial,
	)
	if err != nil {
		// ownership transfers for every non-nil result, including a rejected
		// one
		if hopConn != nil {
			hopConn.Close()
		}
		return nil, nil, err
	}
	if hopConn == nil || hopResponse == nil {
		if hopConn != nil {
			hopConn.Close()
		}
		return nil, nil, fmt.Errorf("the hop dial returned no connection or no response")
	}
	return hopConn, hopResponse, nil
}

// Holds a hop whose dial failed for NLayerHoldTimeout. A hold that is placed
// while the hop is already held, by a connection that picked it before the
// first failure, extends it without being reported again.
func (self *ExtenderServer) holdNLayerHop(index int, err error) {
	newlyHeld := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		hop := self.nlayerHops[index]
		hop.failedCount += 1
		if holdTimeout := self.settings.NLayerHoldTimeout; 0 < holdTimeout {
			hop.heldUntil = time.Now().Add(holdTimeout)
			newlyHeld = !hop.held
			hop.held = true
		}
	}()
	if newlyHeld && self.settings.NLayerHoldHandler != nil {
		self.settings.NLayerHoldHandler(index, true, err)
	}
}

// What this extender has seen of each NLayer hop, in the order of NLayerHops
// (A11). Empty on an extender with no hop.
func (self *ExtenderServer) NLayerStats() []ExtenderNLayerHopStats {
	now := time.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	hopStats := make([]ExtenderNLayerHopStats, 0, len(self.nlayerHops))
	for _, hop := range self.nlayerHops {
		stats := ExtenderNLayerHopStats{
			RelayCount:   hop.relayCount,
			RefusedCount: hop.refusedCount,
			FailedCount:  hop.failedCount,
			LimitedCount: hop.limitedCount,
		}
		if now.Before(hop.heldUntil) {
			stats.HeldUntil = hop.heldUntil
		}
		if now.Before(hop.limitedUntil) {
			stats.LimitedUntil = hop.limitedUntil
		}
		hopStats = append(hopStats, stats)
	}
	return hopStats
}
