package connect

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// net_extender_dns_ports.go -- the dns carrier's ports as a client dials them
// (EXTENDER.md L2).
//
// Every extender binds the dns carrier on 4053. Only the sn miner, a service
// that can take 53, binds 53 beside it; every app binds 4053 alone, on every
// platform. A client tries both on every extender, whether or not its record
// lists 53. The ports a record lists go first, ascending, so a record that
// lists 53 is dialed on 53 first; then whichever of 4053 and 53 the record
// does not list, 4053 first. An app extender's record lists 4053 alone, and
// an address with no record is dialed on the defaults, so both are dialed on
// 4053 first and 53 delays neither.
//
// The ports are one dial, not a dialer each. The dial launches its first port
// at once and each next port when the attempts out have neither answered nor
// failed within one stagger, or at once when they have all failed; the first
// to answer wins, and every other attempt is canceled and joined before the
// dial returns. So a port that never answers costs at most one stagger, never
// a whole attempt, and an extender that answers on its first port is reached
// as fast as if that were its only port. The outcome the strategy and the
// directory record stays the carrier's: a 53 that never answers on an app
// extender neither holds the address nor drops the dialer that reached it on
// 4053, and it takes no second slot of the strategy's parallel block or
// expand budget. This is the race the platform's alt dns carrier runs its two
// ports through (raceAltQuicDial), at the same stagger.

// The launch stagger of the dns carrier's ports (L2), which is what the
// platform's alt dns carrier staggers its ports by
// (platformH3FamilyRaceStagger).
const extenderDnsPortRaceStagger = 250 * time.Millisecond

// The ports one dns carrier dial races, in launch order: DnsPorts without a
// port out of range or repeated, else Profile.Port alone.
func (self *ExtenderConfig) dnsDialPorts() []int {
	dnsPorts := []int{}
	for _, dnsPort := range self.DnsPorts {
		if dnsPort <= 0 || 65535 < dnsPort || slices.Contains(dnsPorts, dnsPort) {
			continue
		}
		dnsPorts = append(dnsPorts, dnsPort)
	}
	if len(dnsPorts) == 0 {
		return []int{self.Profile.Port}
	}
	return dnsPorts
}

// The dns carrier: dialExtenderQuic over the dns packet translation, raced
// over the config's dns ports. Each attempt dials, and names in its request,
// its own port.
func dialExtenderDns(
	ctx context.Context,
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
	extenderTlsConfig *tls.Config,
	headerBytes []byte,
	roundTrip *ExtenderRoundTrip,
) (net.Conn, *protocol.ExtenderResponse, error) {
	stagger := func() <-chan time.Time {
		return time.After(extenderDnsPortRaceStagger)
	}
	dialPort := func(
		ctx context.Context,
		dnsPort int,
		roundTrip *ExtenderRoundTrip,
	) (net.Conn, *protocol.ExtenderResponse, error) {
		portConfig := *extenderConfig
		portConfig.Profile.Port = dnsPort
		portConfig.DnsPorts = nil
		return dialExtenderQuic(ctx, connectSettings, &portConfig, extenderTlsConfig, headerBytes, roundTrip)
	}
	return raceExtenderDnsPorts(ctx, extenderConfig.dnsDialPorts(), roundTrip, stagger, dialPort)
}

// One attempt of a dns port race and what it came back with.
type extenderDnsPortAttempt struct {
	dnsPort int
	// how many attempts had ended when this one launched, so a memory refusal
	// can tell whether an attempt has freed what it held since
	launchEndCount int
	conn           net.Conn
	response       *protocol.ExtenderResponse
	roundTrip      *ExtenderRoundTrip
	err            error
}

// Races one dns carrier dial over `dnsPorts` in order (see the file header):
// the first port at once, each next port one stagger after the last launch
// while every attempt out is pending, and at once when every attempt out has
// failed. The first port to answer wins, and its round trip is copied into
// `roundTrip`. An answer that refuses or limits the request (A4, A12) ends the
// race with that answer, since the extender gives the same one on every port.
// A port the memory budget refused is not a port that failed, since a local
// refusal says nothing about the port: it goes again once an attempt that
// held the budget ends, or at once when one already ended after it launched.
// Every other attempt is canceled and joined before the race returns, and a
// loser that connected anyway is closed, so no loser holds its carrier memory
// past the race. With every port failed the errors are joined in the order
// the attempts ended. `stagger` and `dialPort` are the clock and the dial,
// which a test replaces.
func raceExtenderDnsPorts(
	ctx context.Context,
	dnsPorts []int,
	roundTrip *ExtenderRoundTrip,
	stagger func() <-chan time.Time,
	dialPort func(ctx context.Context, dnsPort int, roundTrip *ExtenderRoundTrip) (net.Conn, *protocol.ExtenderResponse, error),
) (net.Conn, *protocol.ExtenderResponse, error) {
	if len(dnsPorts) == 0 {
		return nil, nil, errors.New("the extender dns carrier has no port")
	}
	if len(dnsPorts) == 1 {
		return dialPort(ctx, dnsPorts[0], roundTrip)
	}

	raceCtx, raceCancel := context.WithCancel(ctx)
	var attemptWorkers sync.WaitGroup
	defer func() {
		raceCancel()
		attemptWorkers.Wait()
	}()

	// only the race loop takes an attempt; one that ends after the race
	// returned closes what it connected itself
	attempts := make(chan *extenderDnsPortAttempt)
	queuedDnsPorts := slices.Clone(dnsPorts)
	pendingCount := 0
	// attempts that ended having held their memory, which is what a port the
	// budget refused waits for
	endCount := 0
	// the queue's head was refused memory while an attempt held it, and waits
	// for an attempt to end
	waitForCapacity := false
	errs := []error{}

	launch := func() {
		attempt := &extenderDnsPortAttempt{
			dnsPort:        queuedDnsPorts[0],
			launchEndCount: endCount,
		}
		queuedDnsPorts = queuedDnsPorts[1:]
		if roundTrip != nil {
			attempt.roundTrip = &ExtenderRoundTrip{}
		}
		pendingCount += 1
		attemptWorkers.Add(1)
		go func() {
			defer attemptWorkers.Done()
			HandleError(func() {
				attempt.conn, attempt.response, attempt.err = dialPort(
					raceCtx,
					attempt.dnsPort,
					attempt.roundTrip,
				)
			}, func(err error) {
				attempt.err = err
			})
			select {
			case attempts <- attempt:
			case <-raceCtx.Done():
				if attempt.conn != nil {
					attempt.conn.Close()
				}
			}
		}()
	}
	// the stagger runs only while a queued port may launch on it
	var staggerC <-chan time.Time
	armStagger := func() {
		staggerC = nil
		if 0 < len(queuedDnsPorts) && !waitForCapacity {
			staggerC = stagger()
		}
	}

	launch()
	armStagger()
	for {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-staggerC:
			launch()
			armStagger()
		case attempt := <-attempts:
			pendingCount -= 1
			if attempt.err != nil && attempt.conn != nil {
				// ownership transfers for every non-nil result, including a
				// rejected one
				attempt.conn.Close()
			}
			var limitedErr *ExtenderLimitedError
			var refusedErr *ExtenderRefusedError
			switch {
			case attempt.err == nil:
				if roundTrip != nil {
					*roundTrip = *attempt.roundTrip
				}
				return attempt.conn, attempt.response, nil
			case errors.As(attempt.err, &limitedErr), errors.As(attempt.err, &refusedErr):
				return nil, nil, attempt.err
			case errors.Is(attempt.err, errExtenderMemoryBudget) &&
				(0 < pendingCount || attempt.launchEndCount < endCount):
				queuedDnsPorts = slices.Insert(queuedDnsPorts, 0, attempt.dnsPort)
				if attempt.launchEndCount < endCount {
					// an attempt already ended after this one launched
					launch()
				} else {
					waitForCapacity = true
				}
				armStagger()
			default:
				errs = append(errs, attempt.err)
				if !errors.Is(attempt.err, errExtenderMemoryBudget) {
					endCount += 1
				}
				waitForCapacity = false
				if 0 < len(queuedDnsPorts) {
					// a failure launches the next port at once
					launch()
					armStagger()
				} else if pendingCount == 0 {
					return nil, nil, errors.Join(errs...)
				}
			}
		}
	}
}
