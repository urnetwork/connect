package connect

// transport_client_limit.go -- the hold a platform transport takes after the
// platform closes it for the network's client limit.
//
// The platform enforces a network's concurrent client limit by closing an
// over-limit connection with the client limit close: on h1 (WebSocket and
// h1+) a 5-byte TransportControlClose message carrying
// TransportCloseReasonClientLimitExceeded, then, on WebSocket only, a close
// frame with ClientLimitCloseCode; on h3 and the dns-carried h3 modes a QUIC
// application close with ClientLimitCloseCode. Any other close, and a close
// control with an unknown reason, is an ordinary close.
//
// Reconnecting at once would only be closed again, so the close starts a hold
// of at least ClientLimitBackoffTimeout. The hold applies to the client, not to
// the one connection the platform closed: no transport sharing the hold dials
// until it ends, whatever mode it runs, and the live connections of the
// transports sharing it close too. A network change does not end it, since the
// limit belongs to the account network and not to the physical path.
//
// One hold is shared by every transport of one client. A provider's transport
// group passes the same hold to its v4, v6 and standby transports, and an owner
// that replaces transports across generations (a migration) passes the same
// hold to each generation through PlatformTransportSettings, so a replacement
// never dials through a hold its predecessor started. A transport whose
// settings carry none owns a private hold.
//
// The platform judges a connection by the provide intent it declared
// (transport_provide_intent.go), so a hold follows the declaration. A close of
// a connection whose declaration the client has since changed is evidence
// about a declaration the client no longer makes: it is an ordinary close, and
// the next dial carries the current declaration. An owner that changes its
// declaration resets the hold (Reset), since the platform has not judged the
// new one yet, and the reset supersedes every connection dialed before it:
// each connection carries the reset generation it dialed under, and a close
// of an older generation starts no hold. The generation is compared under the
// hold's own lock, so a close racing the reset can neither survive it nor
// start a hold after it. A device's first transports dial before it knows its
// provide mode, so without these rules a close of that first undeclared
// connection would hold off the declared one that replaces it.
//
// Concurrency: a ClientLimitBackoff is safe for concurrent use. Its status is
// a MonitorValue, so a reader subscribes and reads in one step. The expiry
// timer is armed only while a hold is in force, so an idle hold costs no
// goroutine and no timer.

import (
	"context"
	"encoding/binary"
	"errors"
	mathrand "math/rand"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	quic "github.com/quic-go/quic-go"
)

// TransportCloseReasonClientLimitExceeded is the TransportControlClose reason
// of the client limit close. Every other reason is reserved and reads as an
// ordinary close.
const TransportCloseReasonClientLimitExceeded uint32 = 1

// ClientLimitCloseCode is the WebSocket close code and the QUIC application
// error code of the client limit close.
const ClientLimitCloseCode = 4001

// ClientLimitCloseText is the close text that accompanies ClientLimitCloseCode.
const ClientLimitCloseText = "client limit exceeded"

// transportCloseReason reads a TransportControlClose message. ok is false for
// every other message.
func transportCloseReason(message []byte) (reason uint32, ok bool) {
	if len(message) != 5 || message[0] != TransportControlClose {
		return 0, false
	}
	return binary.BigEndian.Uint32(message[1:5]), true
}

// isClientLimitCloseError reports whether a carrier error is the platform's
// client limit close: a WebSocket close frame with ClientLimitCloseCode, or a
// QUIC application close from the remote with ClientLimitCloseCode.
func isClientLimitCloseError(err error) bool {
	if err == nil {
		return false
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) && closeErr.Code == ClientLimitCloseCode {
		return true
	}
	var applicationErr *quic.ApplicationError
	if errors.As(err, &applicationErr) && applicationErr.Remote &&
		applicationErr.ErrorCode == ClientLimitCloseCode {
		return true
	}
	return false
}

// ClientLimitBackoffTimeout is the minimum hold after a client limit close.
const ClientLimitBackoffTimeout = 15 * time.Minute

// ClientLimitBackoffJitter bounds the random extension of a hold, so clients
// closed together do not return together.
const ClientLimitBackoffJitter = 5 * time.Minute

// ClientLimitStatus is one readout of a client limit hold.
type ClientLimitStatus struct {
	// Exceeded is true while the hold is in force.
	Exceeded bool
	// RetryTime is when the hold ends and dials resume. Zero while not
	// exceeded.
	RetryTime time.Time
}

// ClientLimitBackoff is the client limit hold shared by the platform
// transports of one client. See the file header.
type ClientLimitBackoff struct {
	status *MonitorValue[ClientLimitStatus]

	stateLock sync.Mutex
	// stops the expiry of the hold in force; nil while none is armed
	stopExpire func() bool
	// increments on every arm, so a timer that already fired for an older
	// hold cannot end or re-arm a newer one
	expireGeneration uint64
	// increments on every Reset; a connection carries the value it dialed
	// under (dialResetGeneration)
	resetGeneration uint64

	// the clock, replaced only by package tests
	now       func() time.Time
	afterFunc func(timeout time.Duration, f func()) (stop func() bool)
	jitter    func(maxJitter time.Duration) time.Duration
}

// NewClientLimitBackoff returns a hold that is not in force.
func NewClientLimitBackoff() *ClientLimitBackoff {
	return &ClientLimitBackoff{
		status: NewMonitorValue(ClientLimitStatus{}),
		now:    time.Now,
		afterFunc: func(timeout time.Duration, f func()) func() bool {
			return time.AfterFunc(timeout, f).Stop
		},
		jitter: func(maxJitter time.Duration) time.Duration {
			if maxJitter <= 0 {
				return 0
			}
			return time.Duration(mathrand.Int63n(int64(maxJitter)))
		},
	}
}

// Status is the current readout.
func (self *ClientLimitBackoff) Status() ClientLimitStatus {
	return self.status.Value()
}

// Get returns the current readout and a channel that closes on its next
// change, taken in one step. Act on the readout, then wait on the channel.
func (self *ClientLimitBackoff) Get() (ClientLimitStatus, chan struct{}) {
	return self.status.Get()
}

// dialResetGeneration is the generation a connection dials under.
func (self *ClientLimitBackoff) dialResetGeneration() uint64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.resetGeneration
}

// noteExceeded starts a hold for a close of a connection that dialed under
// resetGeneration, or keeps the one in force when it already ends later, and
// returns when the hold ends. held is false, and nothing changes, when a Reset
// came after the dial.
func (self *ClientLimitBackoff) noteExceeded(resetGeneration uint64) (retryTime time.Time, held bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if resetGeneration != self.resetGeneration {
		return time.Time{}, false
	}
	now := self.now()
	retryTime = now.Add(ClientLimitBackoffTimeout + self.jitter(ClientLimitBackoffJitter))
	if current := self.status.Value(); current.Exceeded && !retryTime.After(current.RetryTime) {
		// a second close inside the hold never shortens it
		return current.RetryTime, true
	}
	self.status.Set(ClientLimitStatus{
		Exceeded:  true,
		RetryTime: retryTime,
	})
	self.armExpireWithLock(retryTime.Sub(now))
	return retryTime, true
}

// Reset ends a hold in force at once and supersedes every connection dialed
// before it, for an owner whose next dial carries a declaration the platform
// has not judged yet: a changed provide intent. The transports parked on the
// hold dial again, and a client limit close of a connection dialed before the
// reset starts no hold.
func (self *ClientLimitBackoff) Reset() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.resetGeneration += 1
	if self.stopExpire != nil {
		self.stopExpire()
		self.stopExpire = nil
	}
	// a timer that fired before the stop can no longer end a later hold
	self.expireGeneration += 1
	self.status.Set(ClientLimitStatus{})
}

// armExpireWithLock replaces the expiry timer.
func (self *ClientLimitBackoff) armExpireWithLock(timeout time.Duration) {
	if self.stopExpire != nil {
		self.stopExpire()
	}
	self.expireGeneration += 1
	expireGeneration := self.expireGeneration
	self.stopExpire = self.afterFunc(timeout, func() {
		self.expire(expireGeneration)
	})
}

// expire ends the hold once its retry time has come. A timer that fired early
// re-arms for the remainder; a timer of an older arm does nothing.
func (self *ClientLimitBackoff) expire(expireGeneration uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if expireGeneration != self.expireGeneration {
		return
	}
	current := self.status.Value()
	if !current.Exceeded {
		self.stopExpire = nil
		return
	}
	if remaining := current.RetryTime.Sub(self.now()); 0 < remaining {
		self.armExpireWithLock(remaining)
		return
	}
	self.stopExpire = nil
	self.status.Set(ClientLimitStatus{})
}

// noteClientLimitClose starts the client's hold after the platform's client
// limit close of this transport's connection in mode, which declared
// provideIntent and carried resetGeneration when it dialed. A connection whose
// declaration the client has since changed, or that dialed before the hold's
// last reset, starts no hold (see the file header).
func (self *PlatformTransport) noteClientLimitClose(
	mode TransportMode,
	provideIntent bool,
	resetGeneration uint64,
) {
	if self.clientLimitBackoff == nil {
		return
	}
	superseded := func() {
		self.log.Infof(
			"[t]%s connection closed by the platform: client limit exceeded for a provide intent the client no longer declares; redialing with the current one\n",
			mode,
		)
	}
	if self.authSnapshot().ProvideIntent != provideIntent {
		superseded()
		return
	}
	retryTime, held := self.clientLimitBackoff.noteExceeded(resetGeneration)
	if !held {
		superseded()
		return
	}
	self.log.Infof(
		"[t]%s connection closed by the platform: client limit exceeded; dials hold until %s\n",
		mode,
		retryTime.Format(time.RFC3339),
	)
}

// clientLimitResetGeneration is the reset generation of the transport's hold,
// which a connection carries from its dial. Read it after the auth snapshot of
// the dial, so a connection never pairs a declaration with a generation older
// than the reset that came with it.
func (self *PlatformTransport) clientLimitResetGeneration() uint64 {
	if self.clientLimitBackoff == nil {
		return 0
	}
	return self.clientLimitBackoff.dialResetGeneration()
}

// clientLimitHold reads the transport's client limit hold and a channel that
// closes on its next change. A transport a fixture built without its
// constructor has no hold: it reads none and a channel that never closes.
func (self *PlatformTransport) clientLimitHold() (ClientLimitStatus, chan struct{}) {
	if self.clientLimitBackoff == nil {
		return ClientLimitStatus{}, nil
	}
	return self.clientLimitBackoff.Get()
}

// runConnectionWatch closes a live connection on a network change kick, so the
// loop re-dials over the new path at once, or when a client limit hold is in
// force, so the connection stops with the one the platform closed. Closing the
// carrier is what unblocks a reader or writer parked in socket I/O that
// cancelling handleCtx alone cannot wake. Returns when the connection ends.
func (self *PlatformTransport) runConnectionWatch(
	handleCtx context.Context,
	kick chan struct{},
	closeConnection func(reason string),
) {
	for {
		clientLimitStatus, clientLimitNotify := self.clientLimitHold()
		if clientLimitStatus.Exceeded {
			self.log.Infof("[t]client limit hold: closing connection\n")
			closeConnection("client limit")
			return
		}
		select {
		case <-handleCtx.Done():
			return
		case <-kick:
			self.log.Infof("[t]kick: closing connection for re-dial\n")
			closeConnection("network change")
			return
		case <-clientLimitNotify:
		}
	}
}
