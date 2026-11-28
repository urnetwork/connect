package connect

// transport_h3_budget_fallback.go -- the h1 that a transport whose target is
// an h3 mode carries its client on while the memory budget cannot admit its h3
// claim (server LBALTOPT2 R20).
//
// A transport whose target is h3, h3dns or h3dnspump reserves one complete h3
// graph from its owner's PlatformTransportBudget before it dials. A phone's
// budget holds only a few such graphs, and an explicit h3 claim cannot preempt
// another, so a claim that did not fit waited for budget for as long as its
// transport lived, and its client routed nothing (LBALTOPT2 §3.12, C8). Now the
// claim contends for H3BudgetFallbackTimeout. If the budget has not admitted
// it by then, the transport carries its client on h1, whose claim is small,
// under a context of its own:
//
//   - The h3 claim stays registered but deferred: it takes no precedence over
//     other claims, so it never holds back an h1 claim the budget can admit,
//     its own fallback's among them.
//   - Each change of the budget retries it once without waiting
//     (TryAcquireOrPreempt). A retry may revoke optional Auto-H3 leases when
//     that alone admits the claim, as an explicit claim always could, and
//     the claim keeps its precedence while that preemption runs, so the
//     yielded capacity is its own. A failed retry changes nothing and
//     notifies nobody, so retries cannot wake each other, and a retry costs
//     one admission check per budget change. There is no retry timer.
//   - Once the budget admits the claim, the h3 runner dials beside the h1.
//     The target ranks above h1, so h1 drains once h3 carries traffic
//     (runInactiveDrain); when h1 carries nothing more the fallback stops and
//     releases its claim. Until h3 connects, h1 keeps carrying the client. An
//     explicit h3 claim is never yielded once admitted, so a transport falls
//     back at most once and returns to h3 at most once in its life.
//
// A held transport does not contend for its claim either: the claim is
// deferred while the owner or the pinned family holds the transport, so a
// provider's held standby reserves no graph it is not using.
//
// Safe for concurrent use: the fallback's state is guarded by its stateLock.

import (
	"context"
	"sync"
	"time"
)

type platformH3BudgetFallback struct {
	transport *PlatformTransport
	timeout   time.Duration

	stateLock sync.Mutex
	// set when the transport falls back, and never cleared
	engaged bool
	// set when the admitted target took over and the h1 stopped for good
	retired bool
	// set when the retirement watcher started
	retiring bool
	// the fallback's own h1 claim and the cancel of its h1 runner's context,
	// both set with engaged
	h1Claim *platformTransportBudgetReservation
	cancel  context.CancelFunc
}

func newPlatformH3BudgetFallback(transport *PlatformTransport, timeout time.Duration) *platformH3BudgetFallback {
	return &platformH3BudgetFallback{
		transport: transport,
		timeout:   timeout,
	}
}

// acquire contends for the transport's h3 claim until the budget admits it,
// and falls back to h1 once the timeout passes without admission. It returns
// admitted when the claim holds its capacity, and parked when the transport
// became held: the claim is deferred again, and the caller parks in admission
// before it contends again. Neither means ctx ended. Called only from the
// transport's h3 runner.
func (self *platformH3BudgetFallback) acquire(
	ctx context.Context,
	reservation *platformTransportBudgetReservation,
) (admitted bool, parked bool) {
	if reservation.IsAcquired() {
		// an explicit claim stays acquired across holds
		return true, false
	}
	// the timeout runs from the first contention after a park
	var deadline time.Time
	for {
		held, heldNotify := self.transport.heldNotify()
		if held {
			reservation.SetDeferred(true)
			return false, true
		}
		// within the timeout this is one pass of Acquire, contending with
		// precedence; on the fallback the pass defers the claim unless its
		// own preemption is admitting it
		engaged := self.isEngaged()
		acquired, _, capacityNotify := reservation.TryAcquireOrPreempt(engaged)
		if acquired {
			if engaged {
				self.retireAfterTarget()
			}
			return true, false
		}
		if ctx.Err() != nil {
			return false, false
		}
		var timeout <-chan time.Time
		if !engaged {
			now := time.Now()
			if deadline.IsZero() {
				deadline = now.Add(self.timeout)
			}
			remaining := deadline.Sub(now)
			if remaining <= 0 {
				self.engage(reservation)
				continue
			}
			timeout = time.After(remaining)
		}
		select {
		case <-ctx.Done():
			return false, false
		case <-capacityNotify:
		case <-heldNotify:
		case <-timeout:
		}
	}
}

// engage starts the h1 fallback: its claim registers now, before the next
// admission check reads the budget, and its runner waits for that claim and
// then carries the client on h1 until the fallback stops.
func (self *platformH3BudgetFallback) engage(reservation *platformTransportBudgetReservation) {
	transport := self.transport
	reservation.SetDeferred(true)
	h1Claim := transport.settings.PlatformTransportBudget.register(
		platformTransportBudgetH1,
		transport.h1BudgetByteCount(),
		true,
	)
	ctx, cancel := context.WithCancel(transport.ctx)
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.engaged = true
		self.h1Claim = h1Claim
		self.cancel = cancel
	}()
	transport.log.Infof("[t]%s memory budget did not admit the claim; carrying on h1 until it does\n", transport.targetMode)
	// the h3 runner calling this is a registered runner, so the add cannot
	// race run's join
	transport.runWaitGroup.Add(1)
	go HandleError(func() {
		defer transport.runWaitGroup.Done()
		defer cancel()
		defer h1Claim.Release()
		if !h1Claim.Acquire(ctx) {
			return
		}
		transport.runH1(ctx, 0)
		// as run does for the main h1: keep the claim until every receive
		// credit of the h1 graph has come back
		if retained := transport.h1RetainedBudget; retained != nil {
			retained.receive.waitReleased()
		}
	}, transport.cancel)
}

// retireAfterTarget stops the fallback once the admitted target carries
// traffic and h1 carries nothing: the target is the active mode and h1's last
// connection has drained and closed. Until then h1 keeps carrying the client,
// so a target that does not connect leaves it on h1.
func (self *platformH3BudgetFallback) retireAfterTarget() {
	transport := self.transport
	var cancel context.CancelFunc
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.engaged || self.retiring {
			return
		}
		self.retiring = true
		cancel = self.cancel
	}()
	if cancel == nil {
		return
	}
	transport.runWaitGroup.Add(1)
	go HandleError(func() {
		defer transport.runWaitGroup.Done()
		for {
			active, modeNotify := transport.activeMode()
			available, availableNotify := transport.modesAvailable()
			if active == transport.targetMode && !available[TransportModeH1] {
				break
			}
			select {
			case <-transport.ctx.Done():
				return
			case <-modeNotify:
			case <-availableNotify:
			}
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.retired = true
		}()
		cancel()
		transport.log.Infof("[t]%s admitted by the memory budget and carrying; h1 fallback stopped\n", transport.targetMode)
	}, transport.cancel)
}

func (self *platformH3BudgetFallback) isEngaged() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.engaged
}

// h1ClaimIfEngaged is the fallback's h1 claim while h1 carries, or may carry,
// the client: from engaging until the target took over.
func (self *platformH3BudgetFallback) h1ClaimIfEngaged() (*platformTransportBudgetReservation, bool) {
	if self == nil {
		return nil, false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.engaged || self.retired {
		return nil, false
	}
	return self.h1Claim, true
}

// H3BudgetFallback reports whether the transport's target is an h3 mode the
// memory budget did not admit in time, so that its client is carried on h1:
// from the fallback engaging until the admitted target carries traffic and
// h1 stops (transport_h3_budget_fallback.go). ActiveMode reads h1 meanwhile.
func (self *PlatformTransport) H3BudgetFallback() bool {
	_, engaged := self.h3BudgetFallback.h1ClaimIfEngaged()
	return engaged
}
