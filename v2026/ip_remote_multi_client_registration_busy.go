// A window's first exit when its mints meet a busy registration bound.
//
// A hosted proxy device mints every window client through its process's
// in-process authority, which admits a few registrations at once and refuses
// the rest with a 429 (server localclient.MintAdmission). The window read that
// refusal as a platform rate limit and backed off 5 s, doubling: on Main from
// 2026-11-14, one, two or three refusals cost a new proxy's first CONNECT about
// 4, 11 or 27 s, and a pass that waited on them deferred the next expansion by
// another 5-10 s (server proxy/P90SPEED.md §3.3).
//
// A busy bound frees a slot within one registration's hold, tens of
// milliseconds. Its refusal now says so (ClientRegistrationBusyError), and:
//   - a window with no usable exit retries it after
//     MultiClientSettings.WindowRegistrationBusyRetry (jittered), at most
//     WindowRegistrationBusyRetryMaxCount times until it has a usable exit
//     again and at most that many in any WindowRegistrationBusyRetryPeriod.
//     Past either bound, and in a window that has an exit, the refusal takes
//     the rate-limit backoff, which these retries never advance. Any other
//     rate limit (the platform API's per-client budgets, a load balancer)
//     keeps the backoff, so an app is never re-asked faster against a quota.
//   - an expansion pass that tried no provider because the window's mints were
//     refused for rate is no evidence against providers: it neither starts nor
//     advances the expansion backoff (ip_remote_multi_client_discovery_retry.go).
//     The mint retry's own cadence above paces the window, and a pass that
//     tries no provider does no work of its own.
package connect

import (
	"context"
	"errors"
	"time"
)

// A derived-client registration refused because an in-process authority's
// concurrent registration bound was full: server localclient's mint admission
// refusal has the method, which errors.As finds anywhere in a wrapped chain.
// Such a refusal still reads as a rate limit by its own text (a 429). The
// authority needs no connect symbol for it, so neither repository's build
// depends on the other's version of this contract.
type ClientRegistrationBusyError interface {
	error
	// true when a slot of the bound frees within one registration's hold
	ClientRegistrationBusy() bool
}

// Whether `err` is a busy registration bound's refusal.
func isClientRegistrationBusy(err error) bool {
	var busy ClientRegistrationBusyError
	return errors.As(err, &busy) && busy.ClientRegistrationBusy()
}

// Why the enumerator last had no candidate to offer.
const (
	// it offered one, or has not failed yet
	mintStarveNone int32 = iota
	// its last mint was refused for rate, and it waits to ask again
	mintStarveRefused
	// a listing failed or found nothing, or a mint failed otherwise
	mintStarveOther
)

// The short retries a window took. Guarded by the window's stateLock.
type registrationBusyRetryState struct {
	// taken since the window last had a usable exit
	spellCount int
	// when the retries of the last period were taken, oldest first
	retryTimes []time.Time
}

// Waits before a failed mint is asked again: the short retry for a busy
// registration bound while the window has no usable exit (see the file
// comment), else windowRetryDelay through waitWindowRetry. False when ctx
// ended first.
func (self *multiClientWindow) waitMintRetry(
	ctx context.Context,
	class windowFailureClass,
	err error,
	networkChanged <-chan struct{},
) bool {
	if class == windowFailureRateLimit && isClientRegistrationBusy(err) {
		if delay, ok := self.takeRegistrationBusyRetry(time.Now()); ok {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(delay):
				return true
			}
		}
	}
	return self.waitWindowRetry(ctx, class, networkChanged)
}

// The jittered short retry delay, if the window may take one now. Taking it
// counts it against both bounds.
func (self *multiClientWindow) takeRegistrationBusyRetry(now time.Time) (time.Duration, bool) {
	// bare fixture windows carry no settings
	if self.settings == nil {
		return 0, false
	}
	retry := self.settings.WindowRegistrationBusyRetry
	maxCount := self.settings.WindowRegistrationBusyRetryMaxCount
	period := self.settings.WindowRegistrationBusyRetryPeriod
	if retry <= 0 || maxCount <= 0 {
		return 0, false
	}
	usable := self.hasUsableExit()
	taken := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		state := &self.registrationBusyRetry
		if usable {
			// an exit ended the empty spell; the next one starts over
			state.spellCount = 0
			return false
		}
		if maxCount <= state.spellCount {
			return false
		}
		if 0 < period {
			expired := 0
			for expired < len(state.retryTimes) && !now.Before(state.retryTimes[expired].Add(period)) {
				expired += 1
			}
			state.retryTimes = state.retryTimes[expired:]
			if maxCount <= len(state.retryTimes) {
				return false
			}
			state.retryTimes = append(state.retryTimes, now)
		}
		state.spellCount += 1
		return true
	}()
	if !taken {
		return 0, false
	}
	return self.retryJitter(retry), true
}

// Whether the window holds an installed client that has not ended.
func (self *multiClientWindow) hasUsableExit() bool {
	for _, client := range self.unorderedClients() {
		if !client.IsDone() {
			return true
		}
	}
	return false
}

// Whether the expansion pass that started at `passCandidateCount` built no
// candidate while the enumerator's last mint was refused for rate: the pass
// waited on the refusals and tried no provider.
func (self *multiClientWindow) expandPassOnlyRefusedMints(passCandidateCount uint64) bool {
	if self.expandCandidateCount.Load() != passCandidateCount {
		return false
	}
	if self.mintStarve.Load() != mintStarveRefused {
		return false
	}
	if log := loggerOrDefault(self.log); log.V(1).Enabled() {
		log.Infof("%s\n", relEvent(
			"discovery_expand_refused_mints",
			"window", self.windowName(),
		))
	}
	return true
}
