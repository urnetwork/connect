package connect

// transport_credential_refusal.go -- the hold a platform transport takes after
// the platform definitively refuses the credential it dialed with.
//
// A refusal is definitive when the platform answered the credential itself: a
// terminal 401 or 403 upgrade response, or an authorization close for a revoked
// session, an expired credential or a rejected one (AuthorizationCloseRevoked,
// AuthorizationCloseExpired, AuthorizationCloseRejected), whether it arrives as
// a WebSocket close frame, a TransportControlClose reason or a QUIC application
// close, before or after the auth exchange. An unavailable close
// (AuthorizationCloseUnavailable), a 503 and every transport failure are failed
// checks, not answers, and take no hold.
//
// Redialing a refused credential can only be refused again. A refusal right
// after a WebSocket's 101 looked like a connection that ran and died, so the
// transport redialed it on the reconnect fast path every few seconds for as long
// as it ran, and a refused h1+ dial backed off but never stopped. Now the
// transport holds its dials on that credential and reports the refusal to its
// owner through AuthorizationClosed, which refreshes the credential or signs
// out. The hold ends when the owner installs a different credential (SetAuth)
// or after CredentialRefusalHoldTimeout. The timeout only bounds the cost of a
// refusal the platform got wrong: a transport whose owner cannot replace the
// credential retries it at that pace instead of every few seconds.
//
// The hold belongs to one transport and is keyed by the refused credential, so
// a SetAuth that keeps the credential, such as a provide intent change, keeps
// the hold. The hold is in force before the owner hears of the refusal, so no
// dial with the refused credential can follow the report.
//
// Concurrency: a credentialRefusalHold is safe for concurrent use. Its status
// is a MonitorValue, so a reader subscribes and reads in one step. The expiry
// timer is armed only while a hold is in force.

import (
	"errors"
	mathrand "math/rand"
	"net/http"
	"sync"
	"time"
)

// CredentialRefusalHoldTimeout is the minimum hold after a definitive refusal
// of a credential the owner does not replace.
const CredentialRefusalHoldTimeout = 15 * time.Minute

// CredentialRefusalHoldJitter bounds the random extension of a hold, so clients
// refused together do not return together.
const CredentialRefusalHoldJitter = 5 * time.Minute

// credentialRefusalStatus is one readout of a credential refusal hold.
type credentialRefusalStatus struct {
	// the refused credential; empty while no hold is in force
	byJwt string
	// when the hold ends and the credential may be dialed again
	retryTime time.Time
}

// credentialRefusalHold is a platform transport's hold on one refused
// credential. See the file header.
type credentialRefusalHold struct {
	status *MonitorValue[credentialRefusalStatus]

	stateLock sync.Mutex
	// stops the expiry of the hold in force; nil while none is armed
	stopExpire func() bool
	// increments on every arm, so a timer that already fired for an older
	// hold cannot end a newer one
	expireGeneration uint64

	// the clock, replaced only by package tests
	now       func() time.Time
	afterFunc func(timeout time.Duration, f func()) (stop func() bool)
	jitter    func(maxJitter time.Duration) time.Duration
}

func newCredentialRefusalHold() *credentialRefusalHold {
	return &credentialRefusalHold{
		status: NewMonitorValue(credentialRefusalStatus{}),
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

// get returns the current readout and a channel that closes on its next
// change, taken in one step.
func (self *credentialRefusalHold) get() (credentialRefusalStatus, chan struct{}) {
	return self.status.Get()
}

// refuse starts a hold on byJwt, or keeps the one in force on it when that
// ends later, and returns when the hold ends.
func (self *credentialRefusalHold) refuse(byJwt string) time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	now := self.now()
	retryTime := now.Add(CredentialRefusalHoldTimeout + self.jitter(CredentialRefusalHoldJitter))
	if current := self.status.Value(); current.byJwt == byJwt && !retryTime.After(current.retryTime) {
		// a second refusal inside the hold never shortens it
		return current.retryTime
	}
	self.status.Set(credentialRefusalStatus{
		byJwt:     byJwt,
		retryTime: retryTime,
	})
	if self.stopExpire != nil {
		self.stopExpire()
	}
	self.expireGeneration += 1
	expireGeneration := self.expireGeneration
	self.stopExpire = self.afterFunc(retryTime.Sub(now), func() {
		self.expire(expireGeneration)
	})
	return retryTime
}

// release ends a hold on any credential other than byJwt, the one the owner
// now dials with.
func (self *credentialRefusalHold) release(byJwt string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if current := self.status.Value(); current.byJwt == "" || current.byJwt == byJwt {
		return
	}
	self.endWithLock()
}

// expire ends the hold once its retry time has come. A timer that fired early
// re-arms for the remainder; a timer of an older arm does nothing.
func (self *credentialRefusalHold) expire(expireGeneration uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if expireGeneration != self.expireGeneration {
		return
	}
	current := self.status.Value()
	if current.byJwt == "" {
		self.stopExpire = nil
		return
	}
	if remaining := current.retryTime.Sub(self.now()); 0 < remaining {
		self.expireGeneration += 1
		expireGeneration := self.expireGeneration
		self.stopExpire = self.afterFunc(remaining, func() {
			self.expire(expireGeneration)
		})
		return
	}
	self.stopExpire = nil
	self.status.Set(credentialRefusalStatus{})
}

func (self *credentialRefusalHold) endWithLock() {
	if self.stopExpire != nil {
		self.stopExpire()
		self.stopExpire = nil
	}
	// a timer that fired before the stop can no longer end a later hold
	self.expireGeneration += 1
	self.status.Set(credentialRefusalStatus{})
}

// definitiveAuthorizationClose reports whether an authorization close answered
// the credential, rather than reporting a failed check.
func definitiveAuthorizationClose(cause AuthorizationCloseCause) bool {
	switch cause {
	case AuthorizationCloseRevoked, AuthorizationCloseExpired, AuthorizationCloseRejected:
		return true
	default:
		return false
	}
}

// credentialRefusalCause is the definitive refusal a dial error carries: a
// terminal 401 or 403 upgrade response, or an authorization close that ended
// the dial. Zero for every other error.
func credentialRefusalCause(err error) AuthorizationCloseCause {
	for _, cause := range flattenHttpRequestCauses(err) {
		if upgrade, ok := cause.err.(*HTTPUpgradeError); ok && upgrade != nil && upgrade.Terminal &&
			(upgrade.StatusCode == http.StatusUnauthorized || upgrade.StatusCode == http.StatusForbidden) {
			return AuthorizationCloseRejected
		}
	}
	if cause := authorizationCloseCause(err); definitiveAuthorizationClose(cause) {
		return cause
	}
	var refusal *transportAuthorizationRefusal
	if errors.As(err, &refusal) && definitiveAuthorizationClose(refusal.cause) {
		return refusal.cause
	}
	return 0
}

// transportAuthorizationRefusal is the auth echo of a legacy frame-auth dial
// replaced by the platform's close control for the credential.
type transportAuthorizationRefusal struct {
	cause AuthorizationCloseCause
}

func (self *transportAuthorizationRefusal) Error() string {
	return "Auth response error: credential refused."
}

// noteCredentialRefusal holds this transport's dials on byJwt after the
// platform definitively refused it in mode, then reports the refusal to the
// owner. The comparison with the current credential happens when a runner
// waits for admission, so a credential the owner already replaced dials on.
func (self *PlatformTransport) noteCredentialRefusal(mode TransportMode, byJwt string, cause AuthorizationCloseCause) {
	retryTime := self.credentialRefusalHold.refuse(byJwt)
	self.log.Infof(
		"[t]%s credential refused by the platform (%d); dials hold for a new credential until %s\n",
		mode,
		cause,
		retryTime.Format(time.RFC3339),
	)
	self.noteAuthorizationClose(cause)
}

// noteConnectionAuthorizationClose reports an authorization close of a
// connection that dialed with byJwt, holding dials on the credential when the
// close answered it.
func (self *PlatformTransport) noteConnectionAuthorizationClose(mode TransportMode, byJwt string, cause AuthorizationCloseCause) {
	if definitiveAuthorizationClose(cause) {
		self.noteCredentialRefusal(mode, byJwt, cause)
		return
	}
	self.noteAuthorizationClose(cause)
}

// credentialRefused reports whether the transport's current credential is
// held after a definitive refusal, with a channel that closes when the hold
// changes. A transport a fixture built without its constructor has no hold.
func (self *PlatformTransport) credentialRefused() (bool, chan struct{}) {
	if self.credentialRefusalHold == nil {
		return false, nil
	}
	status, notify := self.credentialRefusalHold.get()
	return status.byJwt != "" && status.byJwt == self.authSnapshot().ByJwt, notify
}
