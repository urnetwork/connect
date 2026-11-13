package connect

import (
	"context"
	mathrand "math/rand"
	"sync"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Retry pacing of a ControlSyncOob. A client's syncs read it from
// `ClientSettings.ControlSyncOobSettings`, and each copies it at construction.
type ControlSyncOobSettings struct {
	// The nominal wait after a send's first failed attempt. It doubles per
	// consecutive failure up to RetryMaxTimeout, and each wait is drawn
	// uniformly from [d/2, d) of the nominal wait d. Each Send starts here
	// again: a success or a newer Send ends the previous send's backoff.
	// Non-positive retries at once.
	RetryTimeout time.Duration
	// Caps the nominal wait, so a refused send still retries at least this
	// often. At or below RetryTimeout, the nominal wait stays RetryTimeout.
	RetryMaxTimeout time.Duration
}

func DefaultControlSyncOobSettings() *ControlSyncOobSettings {
	return &ControlSyncOobSettings{
		// the former fixed wait, so a one-off failure retries about as soon
		RetryTimeout: 1 * time.Second,
		// at the cap a send waits 15 to 30 s: 1000 clients refused together
		// retry about 45 times a second instead of 1000, and each still retries
		// within 30 s of recovery, inside the transport's 60 s dial backoff cap
		RetryMaxTimeout: 30 * time.Second,
	}
}

// ControlSyncOob is like ControlSync, but delivers control messages via the
// client out-of-band control (a synchronous request/response to the platform)
// instead of the in-band client sequence. Because the oob call returns the
// platform's response, its ack means the message was PROCESSED (e.g. a provide
// secret is registered on the platform), not merely delivered — which is the
// reliable "committed" signal a normal control ack does not give. It retries on
// error until success or the context/client is closed, and only the latest
// message per scope is retried. Consecutive failures of a send back off
// exponentially with jitter (ControlSyncOobSettings), so many clients refused
// together neither retry once a second nor in lockstep.
type ControlSyncOob struct {
	ctx    context.Context
	cancel context.CancelFunc

	client   *Client
	scopeTag string
	// a copy, so a test may pace one sync without touching the client's
	settings ControlSyncOobSettings

	sendLock  sync.Mutex
	closed    bool
	syncCount uint64
	// currentCancel owns the in-flight request/retry generation. A newer Send
	// cancels it before starting, so production OOB HTTP work cannot outlive
	// supersession and report a stale success.
	currentCancel context.CancelFunc
	workers       *lifecycleAdmission
	// Nil test barriers expose owned-launcher completion and join entry.
	beforeWorkerDoneForTest func()
	beforeCloseWaitForTest  func()
	beforeSendLockForTest   func()
	// Nil test hooks replace time.After for a retry wait, and the uniform
	// jitter offset in [0, half) of a retry wait.
	retryAfterForTest  func(timeout time.Duration) <-chan time.Time
	retryJitterForTest func(half time.Duration) time.Duration
}

func NewControlSyncOob(ctx context.Context, client *Client, scopeTag string) *ControlSyncOob {
	cancelCtx, cancel := context.WithCancel(ctx)

	// client settings built without the field pace with the defaults
	settings := client.settings.ControlSyncOobSettings
	if settings == nil {
		settings = DefaultControlSyncOobSettings()
	}

	return &ControlSyncOob{
		ctx:      cancelCtx,
		cancel:   cancel,
		client:   client,
		scopeTag: scopeTag,
		settings: *settings,
		workers:  newLifecycleAdmission(),
	}
}

// startWorker admits one owned request launcher or retry delay.
func (self *ControlSyncOob) startWorker(run func()) bool {
	if !self.workers.start() {
		return false
	}
	go func() {
		defer self.workers.finish()
		HandleError(run, self.cancel)
		if self.beforeWorkerDoneForTest != nil {
			self.beforeWorkerDoneForTest()
		}
	}()
	return true
}

// Send delivers `frame` via the client out-of-band control, retrying on error
// until success or the context/client is closed. `ackCallback` fires (once) when
// the platform has processed the message (the oob call returns). The caller's
// `frame` is consumed here.
func (self *ControlSyncOob) Send(frame *protocol.Frame, ackCallback AckFunction) {
	safeAckCallback := func(err error) {
		if ackCallback != nil {
			HandleError(func() {
				ackCallback(err)
			})
		}
	}

	// own a plain copy of the payload so it survives retries (the oob consumes
	// the frame it is given); the caller's frame is returned to the pool here
	messageType := frame.MessageType
	payload := append([]byte(nil), frame.MessageBytes...)
	MessagePoolReturn(frame.MessageBytes)

	handleCtx, handleCancel := context.WithCancel(self.ctx)

	if self.beforeSendLockForTest != nil {
		self.beforeSendLockForTest()
	}
	self.sendLock.Lock()
	if self.closed {
		self.sendLock.Unlock()
		handleCancel()
		return
	}
	previousCancel := self.currentCancel
	self.syncCount += 1
	syncIndex := self.syncCount
	self.currentCancel = handleCancel
	self.sendLock.Unlock()
	if previousCancel != nil {
		previousCancel()
	}

	isCurrent := func() bool {
		self.sendLock.Lock()
		defer self.sendLock.Unlock()
		return syncIndex == self.syncCount && handleCtx.Err() == nil
	}
	claimCurrent := func() bool {
		self.sendLock.Lock()
		if syncIndex != self.syncCount || handleCtx.Err() != nil {
			self.sendLock.Unlock()
			return false
		}
		// Claim completion while the generation comparison is still protected.
		// Splitting this into isCurrent() followed by the ack left a check/act
		// window in which a newer Send could supersede us and the old request
		// would nevertheless publish a stale success.
		self.currentCancel = nil
		self.sendLock.Unlock()
		handleCancel()
		return true
	}

	// The nominal wait before this send's next retry. Attempts of one send run
	// one at a time, each launched from the previous attempt's callback, so
	// only one callback touches it. A success or a newer Send ends this send,
	// and the next send starts again from RetryTimeout.
	nominalRetryTimeout := self.settings.RetryTimeout
	// Draws a failed attempt's wait uniformly from [d/2, d) of the nominal wait
	// d, then doubles d up to RetryMaxTimeout for the next failure (the shape of
	// pinnedDialBackoff). Jitter only downward, so the cap is a real ceiling.
	nextRetryTimeout := func() time.Duration {
		d := nominalRetryTimeout
		if d <= 0 {
			return 0
		}
		maxTimeout := max(self.settings.RetryTimeout, self.settings.RetryMaxTimeout)
		// compare against half the cap, since doubling d could overflow
		if maxTimeout/2 < d {
			nominalRetryTimeout = maxTimeout
		} else {
			nominalRetryTimeout = 2 * d
		}
		half := d / 2
		if half <= 0 {
			return d
		}
		if self.retryJitterForTest != nil {
			return half + max(0, min(half-1, self.retryJitterForTest(half)))
		}
		return half + time.Duration(mathrand.Int63n(int64(half)))
	}

	var send func()
	send = func() {
		// stop if a newer Send for this scope superseded this one, or done
		if !isCurrent() {
			handleCancel()
			return
		}
		select {
		case <-handleCtx.Done():
			handleCancel()
			return
		case <-self.client.Done():
			handleCancel()
			return
		default:
		}

		attemptFrame := &protocol.Frame{
			MessageType:  messageType,
			MessageBytes: MessagePoolCopy(payload),
		}
		callback := func(resultFrames []*protocol.Frame, err error) {
			// Resolve completion and supersession under the same generation
			// lock. An old in-flight request may return after a new Send; it
			// must neither acknowledge the old state nor schedule another retry.
			if err == nil {
				if !claimCurrent() {
					handleCancel()
					return
				}
				// the oob returned: the platform has processed the message
				safeAckCallback(nil)
				return
			}
			if !isCurrent() {
				handleCancel()
				return
			}
			retryTimeout := nextRetryTimeout()
			if self.client.log.V(2).Enabled() {
				self.client.log.Infof("[control-oob][%d]retry scope = %s in %s err = %s\n", syncIndex, self.scopeTag, retryTimeout, err)
			}
			retryAfter := time.After
			if self.retryAfterForTest != nil {
				retryAfter = self.retryAfterForTest
			}
			select {
			case <-handleCtx.Done():
			case <-self.client.Done():
			case <-retryAfter(retryTimeout):
				if !self.startWorker(send) {
					handleCancel()
				}
			}
		}
		if oob, ok := self.client.ClientOob().(OutOfBandControlWithCtx); ok {
			oob.SendControlWithCtx(handleCtx, []*protocol.Frame{attemptFrame}, callback)
			return
		}
		self.client.ClientOob().SendControl(
			[]*protocol.Frame{attemptFrame},
			callback,
		)
	}

	if !self.startWorker(send) {
		handleCancel()
	}
}

// Close prevents later retry launchers and cancels the current request.
func (self *ControlSyncOob) Close() {
	self.cancel()
	self.sendLock.Lock()
	self.closed = true
	currentCancel := self.currentCancel
	self.currentCancel = nil
	self.sendLock.Unlock()
	if currentCancel != nil {
		currentCancel()
	}
	self.workers.close()
}

// closeAndWait joins owned request launchers. An out-of-band implementation
// owns a request and callback after SendControl returns; that external work is
// outside this lifecycle boundary.
func (self *ControlSyncOob) closeAndWait(ctx context.Context) error {
	self.Close()
	if self.beforeCloseWaitForTest != nil {
		self.beforeCloseWaitForTest()
	}
	return waitForLifecycleDone(ctx, self.workers.Done(), "out-of-band control sync workers")
}
