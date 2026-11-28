package connect

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
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
// The ports are one dial, not a dialer each, and the dial races them as alt's
// dns carriers race theirs (quic_dial_race.go). A dns carried dial is slow: a
// ClientHello of about twenty dns queries, paced dns responses, and a round
// trip and a half before the extender's answer, about 0.2 to 0.4 s on loopback
// and up to a second on a phone. So the race never ends an attempt that is
// hearing from the extender:
//
//   - the first port launches at once, and the next one stagger
//     (ConnectSettings.ExtenderDnsRaceStagger, 1 s) after the last launch,
//     beside the attempts out, when the memory budget admits its claim, or at
//     once when every attempt out has failed;
//   - a port the budget refused waits for an attempt to end or to stall. An
//     attempt has stalled once its socket has delivered nothing from the
//     extender for the stall timeout (ExtenderDnsRaceStallTimeout, 2 s), since
//     its launch or its last packet. Then, and only to free its claim for the
//     refused port, the race ends it;
//   - the first to answer wins, and every other attempt is canceled and joined
//     before the dial returns.
//
// So a healthy attempt keeps its whole budget, a blackholed port holds the only
// claim for the stall timeout rather than for its whole handshake bound, and
// the race never holds more claims than the budget admits. The caller's
// deadline bounds the dial, and each attempt keeps its own handshake and
// request bounds (dialExtenderQuic). Before 2026-11-22 the race launched the
// next port at 250 ms, beside most healthy dials, and a silent first port held
// a phone's only claim for its whole handshake bound.
//
// The outcome the strategy and the directory record stays the carrier's: a 53
// that never answers on an app extender neither holds the address nor drops
// the dialer that reached it on 4053, and it takes no second slot of the
// strategy's parallel block or expand budget.

// How one dial of the dns carrier races its ports: as a whodis race
// (quic_dial_race.go), on the extender's stagger and stall timeout.
func (self *ConnectSettings) extenderDnsRaceSchedule() quicDialRaceSchedule {
	return quicDialRaceSchedule{
		stagger:      self.ExtenderDnsRaceStagger,
		whodis:       true,
		stallTimeout: self.ExtenderDnsRaceStallTimeout,
	}
}

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
	return raceExtenderDnsPorts(
		ctx,
		extenderConfig.dnsDialPorts(),
		roundTrip,
		connectSettings.extenderDnsRaceSchedule(),
		dialPort,
	)
}

// One attempt of a dns port race and what it came back with.
type extenderDnsPortAttempt struct {
	dnsPort int
	// the attempt's place in the race's clocks (quicDialRaceAttempts)
	index int
	// how many attempts had ended when this one launched, so a memory refusal
	// can tell whether an attempt has freed what it held since
	launchEndCount int
	conn           net.Conn
	response       *protocol.ExtenderResponse
	roundTrip      *ExtenderRoundTrip
	err            error
}

// Races one dns carrier dial over `dnsPorts` in order (see the file header):
// the first port at once, each next port one stagger after the last launch,
// beside the attempts out, and at once when every attempt out has failed. The
// first port to answer wins, and its round trip is copied into `roundTrip`. An
// answer that refuses or limits the request (A4, A12) ends the race with that
// answer, since the extender gives the same one on every port.
//
// A port the memory budget refused is not a port that failed, since a local
// refusal says nothing about the port: it goes again once an attempt that held
// the budget ends, or at once when one already ended after it launched. While
// it waits, an attempt out that has heard nothing from the extender for the
// stall timeout is ended to free its claim, with errQuicDialStalled as the
// cause its dial returns; an attempt that is hearing from the extender is
// never ended for it.
//
// Every other attempt is canceled and joined before the race returns, and a
// loser that connected anyway is closed, so no loser holds its carrier memory
// past the race. With every port failed the errors are joined in the order the
// attempts ended. `schedule` is a whodis schedule
// (ConnectSettings.extenderDnsRaceSchedule). `dialPort` is the dial, which a
// test replaces: it reports what it hears from the extender to the receive
// clock its context carries (dialExtenderQuic).
//
// The race's clocks are its own. An attempt's clock hears only its own
// attempt, and a clock the caller's context carries hears nothing of the
// extender's carrier: a platform h3dns dial through an extender carries its
// gate lease's clock (platform_h3_gate.go), which times what that dial hears
// from alt, and the carrier's packets, its keepalive acknowledgements
// included, would hold the gate for a dial whose alt is blackholed.
func raceExtenderDnsPorts(
	ctx context.Context,
	dnsPorts []int,
	roundTrip *ExtenderRoundTrip,
	schedule quicDialRaceSchedule,
	dialPort func(ctx context.Context, dnsPort int, roundTrip *ExtenderRoundTrip) (net.Conn, *protocol.ExtenderResponse, error),
) (net.Conn, *protocol.ExtenderResponse, error) {
	if len(dnsPorts) == 0 {
		return nil, nil, errors.New("the extender dns carrier has no port")
	}
	ctx = withQuicDialReceiveClock(ctx, nil)
	if len(dnsPorts) == 1 {
		return dialPort(ctx, dnsPorts[0], roundTrip)
	}

	raceCtx, raceCancel := context.WithCancel(ctx)
	// the cancel and receive clock of each attempt launched; only the race
	// loop touches them
	attemptClocks := &quicDialRaceAttempts{schedule: schedule}
	var attemptWorkers sync.WaitGroup
	defer func() {
		raceCancel()
		attemptClocks.cancelAll()
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
	// for an attempt to end or to stall
	waitForCapacity := false
	errs := []error{}

	launch := func() {
		attemptCtx, index := attemptClocks.launch(raceCtx)
		attempt := &extenderDnsPortAttempt{
			dnsPort:        queuedDnsPorts[0],
			index:          index,
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
					attemptCtx,
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
			staggerC = time.After(schedule.stagger)
		}
	}
	// the stall check runs only while the queue's head waits for capacity: it
	// ends each attempt out that has stalled, and wakes when the next could
	var stallC <-chan time.Time
	checkStalled := func() {
		stallC = nil
		if !waitForCapacity {
			return
		}
		if stallTime := attemptClocks.cancelStalled(time.Now()); !stallTime.IsZero() {
			stallC = time.After(time.Until(stallTime))
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
		case <-stallC:
			checkStalled()
		case attempt := <-attempts:
			pendingCount -= 1
			attemptClocks.ended(attempt.index)
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
					// an attempt out holds the budget: it keeps its claim
					// while it hears from the extender
					waitForCapacity = true
				}
				armStagger()
				checkStalled()
			default:
				errs = append(errs, attempt.err)
				if !errors.Is(attempt.err, errExtenderMemoryBudget) {
					endCount += 1
				}
				waitForCapacity = false
				stallC = nil
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
