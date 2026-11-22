package connect

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"time"
)

// quic_dial_race.go -- what the candidate races of a QUIC dial share
// (raceH3Dial, raceH3DialWithMemory, raceAltQuicDial, raceExtenderDnsPorts):
// how a race paces its candidates and, for a whodis race, when an attempt has
// stalled.
//
// A whodis race is a dns carrier's: the platform's h3dns and h3dnspump modes
// and the api's alt whodis dialer, 53 before 4053 on each family, and an
// extender's dns carrier, over its ports in its record's order (L2). A
// whodis handshake takes about 0.4 to 1 s: a ClientHello of about twenty dns
// queries, about sixteen paced dns responses, about ten more queries for the
// Finished, and a round trip and a half. The direct H3 rule, a 250 ms stagger
// at which a candidate the budget cannot admit ends every attempt out, ended
// nearly every whodis handshake of a phone whose budget held one claim, and
// the front waited out its idle timeout for each (lb_alt, 2026-11-21).
//
// So a whodis race never ends an attempt that is hearing from its peer. Its
// stagger is longer than a healthy handshake; at the stagger the next
// candidate launches beside the attempts out when the budget admits its
// claim, and otherwise waits for an attempt to fail or to stall. An attempt
// has stalled once it has heard nothing from its peer for the stall timeout:
// its socket delivered no packet since its launch or its last packet. That is
// the bound that keeps a blackholed 53 or v6 from holding the only claim to
// the request deadline, while a healthy handshake, which hears alt within a
// round trip of its ClientHello and then every few paced responses, keeps its
// whole budget. The platform's h3 gate holds a dns carrier's dial to the same
// rule against a better mode waiting for the gate (platform_h3_gate.go).
//
// The races run on one goroutine each; only the receive clock is read across
// goroutines.

// How a candidate race paces its candidates.
type quicDialRaceSchedule struct {
	// The delay after a launch before the next candidate launches beside the
	// attempts out. Non-positive launches the next at once.
	stagger time.Duration
	// A whodis race: a candidate the budget cannot admit never ends an attempt
	// that is hearing from its peer. Unset, it ends every attempt out and takes
	// the first claim freed, the direct H3 rule.
	whodis bool
	// For a whodis race, how long an attempt may hear nothing from its peer
	// before it gives up its claim to a candidate the budget refused.
	// Non-positive never ends an attempt for its silence: a refused candidate
	// waits for an attempt to end.
	stallTimeout time.Duration
}

// The cause a race cancels a stalled attempt with, which its dial returns: a
// failure of the path, unlike the cancel of a race's loser.
var errQuicDialStalled = errors.New("quic dial attempt heard nothing from its peer")

// When one dial attempt's socket last delivered a packet from its peer, from
// the clock's start until the first. A race attempt's clock starts at its
// launch, and a platform dial's at its gate lease (platform_h3_gate.go),
// which is also the parent of the clocks of the attempts its race launches.
// Safe for concurrent use: the socket's reader stores, and the race or the
// gate loads.
type quicDialReceiveClock struct {
	startTime time.Time
	// the last delivery, as the time since startTime
	lastReceiveOffset atomic.Int64
	// moved by every delivery this clock records; nil for none
	parent *quicDialReceiveClock
}

func newQuicDialReceiveClock(parent *quicDialReceiveClock) *quicDialReceiveClock {
	return &quicDialReceiveClock{startTime: time.Now(), parent: parent}
}

func (self *quicDialReceiveClock) received() {
	self.lastReceiveOffset.Store(int64(time.Since(self.startTime)))
	if self.parent != nil {
		self.parent.received()
	}
}

// From when the attempt has heard nothing from its peer.
func (self *quicDialReceiveClock) silentSince() time.Time {
	return self.startTime.Add(time.Duration(self.lastReceiveOffset.Load()))
}

type quicDialReceiveClockContextKey struct{}

// The receive clock a dial context carries, nil for none.
func quicDialReceiveClockFromContext(ctx context.Context) *quicDialReceiveClock {
	clock, _ := ctx.Value(quicDialReceiveClockContextKey{}).(*quicDialReceiveClock)
	return clock
}

// The dial context carrying the clock, which the dial's packet conn reports
// to (quicDialReceiveClockPacketConn).
func withQuicDialReceiveClock(ctx context.Context, clock *quicDialReceiveClock) context.Context {
	return context.WithValue(ctx, quicDialReceiveClockContextKey{}, clock)
}

// The packet conn QUIC reads for one attempt, wrapped to report each packet
// it delivers to the receive clock its dial context carries (a whodis race's
// attempt, or a dns carrier's lease), or packetConn itself when the context
// carries none. Only the dns translations call it: QUIC reads a translation
// through ReadFrom either way, where wrapping a UDP socket would cost QUIC
// its batched and ECN reads.
func quicDialReceiveClockPacketConn(ctx context.Context, packetConn net.PacketConn) net.PacketConn {
	clock := quicDialReceiveClockFromContext(ctx)
	if clock == nil {
		return packetConn
	}
	return &quicDialReceiveClockConn{PacketConn: packetConn, clock: clock}
}

// Reports each packet QUIC reads from a dns translation to the receive
// clock. The buffer setters pass through, so QUIC sizes the socket under the
// translation as it does unwrapped.
type quicDialReceiveClockConn struct {
	net.PacketConn
	clock *quicDialReceiveClock
}

func (self *quicDialReceiveClockConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, addr, err := self.PacketConn.ReadFrom(b)
	if err == nil {
		self.clock.received()
	}
	return n, addr, err
}

func (self *quicDialReceiveClockConn) SetReadBuffer(byteCount int) error {
	conn, ok := self.PacketConn.(interface{ SetReadBuffer(int) error })
	if !ok {
		return errors.New("the packet conn has no receive buffer to size")
	}
	return conn.SetReadBuffer(byteCount)
}

func (self *quicDialReceiveClockConn) SetWriteBuffer(byteCount int) error {
	conn, ok := self.PacketConn.(interface{ SetWriteBuffer(int) error })
	if !ok {
		return errors.New("the packet conn has no send buffer to size")
	}
	return conn.SetWriteBuffer(byteCount)
}

// The attempts one race has launched, in launch order. Owned by the race's
// goroutine.
type quicDialRaceAttempts struct {
	schedule quicDialRaceSchedule
	attempts []*quicDialRaceAttempt
}

type quicDialRaceAttempt struct {
	cancel context.CancelCauseFunc
	// a whodis race's; nil otherwise
	clock *quicDialReceiveClock
	// the race has its result
	ended bool
	// canceled for its silence, its result still to come
	stalled bool
}

// The context of the next attempt, canceled with the race and carrying a
// whodis race's receive clock, under the dial's own when it has one, and the
// attempt's index.
func (self *quicDialRaceAttempts) launch(ctx context.Context) (context.Context, int) {
	attemptCtx, cancel := context.WithCancelCause(ctx)
	attempt := &quicDialRaceAttempt{cancel: cancel}
	if self.schedule.whodis {
		attempt.clock = newQuicDialReceiveClock(quicDialReceiveClockFromContext(ctx))
		attemptCtx = withQuicDialReceiveClock(attemptCtx, attempt.clock)
	}
	self.attempts = append(self.attempts, attempt)
	return attemptCtx, len(self.attempts) - 1
}

func (self *quicDialRaceAttempts) ended(index int) {
	self.attempts[index].ended = true
}

func (self *quicDialRaceAttempts) cancelAll() {
	for _, attempt := range self.attempts {
		attempt.cancel(nil)
	}
}

// Ends each attempt out that has heard nothing from its peer for the stall
// timeout, and returns when the next attempt out could stall, zero when none
// can.
func (self *quicDialRaceAttempts) cancelStalled(now time.Time) time.Time {
	var nextStallTime time.Time
	if !self.schedule.whodis || self.schedule.stallTimeout <= 0 {
		return nextStallTime
	}
	for _, attempt := range self.attempts {
		if attempt.ended || attempt.stalled {
			continue
		}
		stallTime := attempt.clock.silentSince().Add(self.schedule.stallTimeout)
		if !now.Before(stallTime) {
			attempt.stalled = true
			attempt.cancel(errQuicDialStalled)
		} else if nextStallTime.IsZero() || stallTime.Before(nextStallTime) {
			nextStallTime = stallTime
		}
	}
	return nextStallTime
}
