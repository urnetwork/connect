package connect

// transport_retry_after.go -- the wait a platform's refusal asks of a mode
// runner's next dial.
//
// The platform's lb_alt front limits each source address (server
// nginx/limit.go). When it refuses an h1 upgrade under a limit, it answers
// 429 Too Many Requests with Retry-After, the whole seconds until the refused
// limit would admit the request, from 1 to 60. When an address holds its cap
// of QUIC connections of a class, the front closes the next one with QUIC
// application error 0x1001 right after the handshake. Many clients behind
// one shared address, a carrier's NAT most of all, share those limits, so a
// client that dials again at once only adds to its address's refusals while
// the clients beside it wait.
//
// So a dial the platform refused that way holds the runner's next dial at
// least:
//   - for a 429 with Retry-After, the seconds it asked for, at most
//     PlatformRetryAfterMaxTimeout, on h1+ and WebSocket alike. The refusal
//     is terminal for its logical dial (newHTTPUpgradeRejection): it is no
//     evidence about the server's protocols, so it never falls back to a
//     fresh WebSocket negotiation at once, and the strategy tries no other
//     route and no next round of the same dial, as it would for a failed
//     route, before the transport's wait;
//   - for a 0x1001 close, a draw uniform in [5 s, 30 s), so the clients behind
//     one address that the cap refused together come back apart, and not
//     before the five seconds an alt reconnect already took to come round.
//
// Either is a floor under the runner's ordinary wait (dialRetryAfter), which
// still applies, as do the pacing and the admission before the next dial; a
// network change still re-dials at once on the new path. A 429 without
// Retry-After, and every other refusal and status, waits as it did.

import (
	"net/http"
	"reflect"
	"time"

	quic "github.com/urnetwork/quic-go"
)

// The longest wait a refusal's Retry-After holds a dial for: the front never
// asks for more, and a longer one from elsewhere is not followed past it.
const PlatformRetryAfterMaxTimeout = 60 * time.Second

// The platform front's QUIC application error for a connection over its
// address's connection cap ("too many connections").
const quicConnectionCapCloseCode quic.ApplicationErrorCode = 0x1001

// The wait after a 0x1001 close: the minimum, then a draw uniform over the
// jitter, so [5 s, 30 s).
const (
	quicConnectionCapRetryMinTimeout = 5 * time.Second
	quicConnectionCapRetryJitter     = 25 * time.Second
)

// A 429's Retry-After, in either form RFC 9110 allows, at most
// PlatformRetryAfterMaxTimeout, and zero when there is none, it doesn't
// parse, or it has passed.
func platformRetryAfter(header http.Header, now time.Time) time.Duration {
	return min(extenderRetryAfter(header, now), PlatformRetryAfterMaxTimeout)
}

// The least wait the error of a refused dial asks of the next dial: the
// longest Retry-After of the 429 refusals among its causes, or for a QUIC
// close with quicConnectionCapCloseCode from the platform, the minimum and a
// jitter draw; zero for every other error. A non-nil draw replaces the jitter
// draw, held to its range, for tests.
func platformDialRefusalWait(err error, draw func(maxJitter time.Duration) time.Duration) time.Duration {
	if err == nil {
		return 0
	}
	wait := time.Duration(0)
	for _, cause := range flattenHttpRequestCauses(err) {
		if upgradeErr, ok := cause.err.(*HTTPUpgradeError); ok && upgradeErr != nil && upgradeErr.StatusCode == http.StatusTooManyRequests {
			wait = max(wait, min(upgradeErr.RetryAfter, PlatformRetryAfterMaxTimeout))
		}
	}
	if quicConnectionCapClosed(err) {
		wait = max(wait, quicConnectionCapRetryMinTimeout+uniformJitter(quicConnectionCapRetryJitter, draw))
	}
	return wait
}

// Whether err's tree holds the platform's QUIC connection cap close, from
// the remote. Every node is looked at, not only the leaves the HTTP causes
// keep: quic-go's ApplicationError unwraps to net.ErrClosed. The walk is
// bounded as the causes' is, and skips a nil cause.
func quicConnectionCapClosed(err error) bool {
	pending := []error{err}
	for visited := 0; len(pending) != 0 && visited < httpRequestCauseNodes; visited += 1 {
		node := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if node == nil {
			continue
		}
		value := reflect.ValueOf(node)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			continue
		}
		if applicationErr, ok := node.(*quic.ApplicationError); ok {
			if applicationErr.Remote && applicationErr.ErrorCode == quicConnectionCapCloseCode {
				return true
			}
			continue
		}
		switch unwrapper := node.(type) {
		case interface{ Unwrap() []error }:
			pending = append(pending, unwrapper.Unwrap()...)
		case interface{ Unwrap() error }:
			pending = append(pending, unwrapper.Unwrap())
		}
	}
	return false
}
