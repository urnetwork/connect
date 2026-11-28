package connect

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The default schedules (DefaultPlatformTransportSettings,
// DefaultClientStrategySettings), which race tests of each kind of candidate
// run under. Literals, so a synctest bubble builds no settings;
// TestQuicDialRaceDefaultSchedules holds them to the defaults.
var testingDirectRaceSchedule = quicDialRaceSchedule{stagger: 250 * time.Millisecond}
var testingWhodisRaceSchedule = quicDialRaceSchedule{
	stagger:      1 * time.Second,
	whodis:       true,
	stallTimeout: 2 * time.Second,
}

// A dns carrier races as whodis and every other H3 mode as direct H3, on the
// platform's and the api's default settings alike.
func TestQuicDialRaceDefaultSchedules(t *testing.T) {
	transport := &PlatformTransport{settings: DefaultPlatformTransportSettings()}
	for _, mode := range []TransportMode{TransportModeH3, TransportModeH3Dns, TransportModeH3DnsPump} {
		want := testingDirectRaceSchedule
		if mode != TransportModeH3 {
			want = testingWhodisRaceSchedule
		}
		if got := transport.h3DialRaceSchedule(mode); got != want {
			t.Errorf("%s schedule=%+v, want %+v", mode, got, want)
		}
	}
	settings := DefaultClientStrategySettings()
	if settings.AltH3RaceStagger != testingDirectRaceSchedule.stagger ||
		settings.AltWhodisRaceStagger != testingWhodisRaceSchedule.stagger ||
		settings.AltWhodisRaceStallTimeout != testingWhodisRaceSchedule.stallTimeout {
		t.Errorf("alt schedules=%s/%s/%s", settings.AltH3RaceStagger, settings.AltWhodisRaceStagger, settings.AltWhodisRaceStallTimeout)
	}
}

// A packet conn whose reads succeed or fail as the test says, and which
// records the buffer sizes it is given.
type receiveClockTestConn struct {
	net.PacketConn
	readErr         error
	readBufferSize  int
	writeBufferSize int
}

func (self *receiveClockTestConn) ReadFrom(b []byte) (int, net.Addr, error) {
	if self.readErr != nil {
		return 0, nil, self.readErr
	}
	return 1, &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 53}, nil
}

func (self *receiveClockTestConn) SetReadBuffer(byteCount int) error {
	self.readBufferSize = byteCount
	return nil
}

func (self *receiveClockTestConn) SetWriteBuffer(byteCount int) error {
	self.writeBufferSize = byteCount
	return nil
}

// A conn with no buffers to size.
type receiveClockTestBareConn struct {
	net.PacketConn
}

// The wrapper moves the clock on each packet it delivers and on nothing
// else, passes the buffer sizes through, and wraps nothing outside a whodis
// race.
func TestQuicDialReceiveClockConnTimesDeliveredPackets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &receiveClockTestConn{}
		if conn := quicDialReceiveClockPacketConn(t.Context(), inner); conn != inner {
			t.Fatal("a dial outside a whodis race was wrapped")
		}
		clock := newQuicDialReceiveClock(nil)
		ctx := context.WithValue(t.Context(), quicDialReceiveClockContextKey{}, clock)
		conn := quicDialReceiveClockPacketConn(ctx, inner)
		if !clock.silentSince().Equal(clock.startTime) {
			t.Fatal("a new clock has heard something")
		}

		time.Sleep(700 * time.Millisecond)
		if _, _, err := conn.ReadFrom(make([]byte, 8)); err != nil {
			t.Fatal(err)
		}
		heard := clock.silentSince()
		if want := clock.startTime.Add(700 * time.Millisecond); !heard.Equal(want) {
			t.Fatalf("silent since %s, want %s", heard.Sub(clock.startTime), want.Sub(clock.startTime))
		}

		time.Sleep(time.Second)
		inner.readErr = errors.New("synthetic read failure")
		if _, _, err := conn.ReadFrom(make([]byte, 8)); err == nil {
			t.Fatal("the failed read succeeded")
		}
		if !clock.silentSince().Equal(heard) {
			t.Fatal("a failed read moved the clock")
		}

		buffers := conn.(interface {
			SetReadBuffer(int) error
			SetWriteBuffer(int) error
		})
		if buffers.SetReadBuffer(4096) != nil || buffers.SetWriteBuffer(8192) != nil ||
			inner.readBufferSize != 4096 || inner.writeBufferSize != 8192 {
			t.Fatalf("buffer sizes did not pass through: %d %d", inner.readBufferSize, inner.writeBufferSize)
		}
		bare := quicDialReceiveClockPacketConn(ctx, &receiveClockTestBareConn{}).(interface {
			SetReadBuffer(int) error
			SetWriteBuffer(int) error
		})
		if bare.SetReadBuffer(4096) == nil || bare.SetWriteBuffer(4096) == nil {
			t.Fatal("a conn with no buffers accepted a size")
		}
	})
}

// A whodis race's attempt clock moves the dial's own clock, which a dns
// carrier's gate lease carries, with every packet it hears; a direct H3
// race's attempts carry the dial's clock untouched.
func TestQuicDialRaceAttemptClocksMoveTheDialClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dialClock := newQuicDialReceiveClock(nil)
		ctx := withQuicDialReceiveClock(t.Context(), dialClock)
		attempts := &quicDialRaceAttempts{schedule: testingWhodisRaceSchedule}
		attemptCtx, index := attempts.launch(ctx)
		attemptClock := quicDialReceiveClockFromContext(attemptCtx)
		if attemptClock == dialClock || attemptClock.parent != dialClock || attempts.attempts[index].clock != attemptClock {
			t.Fatal("the attempt's clock is not under the dial's")
		}
		time.Sleep(time.Second)
		conn := quicDialReceiveClockPacketConn(attemptCtx, &receiveClockTestConn{})
		if _, _, err := conn.ReadFrom(make([]byte, 8)); err != nil {
			t.Fatal(err)
		}
		if !attemptClock.silentSince().Equal(attemptClock.startTime.Add(time.Second)) ||
			!dialClock.silentSince().Equal(dialClock.startTime.Add(time.Second)) {
			t.Fatal("a packet the attempt heard did not move both clocks")
		}
		attempts.cancelAll()

		direct := &quicDialRaceAttempts{schedule: testingDirectRaceSchedule}
		directCtx, _ := direct.launch(ctx)
		if quicDialReceiveClockFromContext(directCtx) != dialClock {
			t.Fatal("a direct H3 attempt lost the dial's clock")
		}
		direct.cancelAll()
	})
}

// Only an attempt out that has heard nothing for the stall timeout is
// ended, with the stall as its cause; the next one that could stall is when
// the silent one would; a direct H3 race never stalls an attempt.
func TestQuicDialRaceAttemptsCancelOnlyStalledAttempts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		attempts := &quicDialRaceAttempts{schedule: testingWhodisRaceSchedule}
		silentCtx, silent := attempts.launch(t.Context())
		hearingCtx, _ := attempts.launch(t.Context())
		endedCtx, ended := attempts.launch(t.Context())
		attempts.ended(ended)
		hearingClock := hearingCtx.Value(quicDialReceiveClockContextKey{}).(*quicDialReceiveClock)

		time.Sleep(1500 * time.Millisecond)
		hearingClock.received()
		launchTime := attempts.attempts[silent].clock.startTime
		if next := attempts.cancelStalled(time.Now()); !next.Equal(launchTime.Add(2 * time.Second)) {
			t.Fatalf("next stall in %s", next.Sub(launchTime))
		}
		if silentCtx.Err() != nil || hearingCtx.Err() != nil {
			t.Fatal("an attempt was ended before its stall timeout")
		}

		time.Sleep(500 * time.Millisecond)
		next := attempts.cancelStalled(time.Now())
		if !errors.Is(context.Cause(silentCtx), errQuicDialStalled) {
			t.Fatalf("silent attempt cause=%v", context.Cause(silentCtx))
		}
		if hearingCtx.Err() != nil || endedCtx.Err() != nil {
			t.Fatal("an attempt that heard alt, or had ended, was stalled")
		}
		if !next.Equal(launchTime.Add(3500 * time.Millisecond)) {
			t.Fatalf("next stall in %s, want the hearing attempt's", next.Sub(launchTime))
		}
		// a stalled attempt is ended once
		if again := attempts.cancelStalled(time.Now()); !again.Equal(next) {
			t.Fatal("the stalled attempt was counted again")
		}
		attempts.cancelAll()
		if !errors.Is(context.Cause(hearingCtx), context.Canceled) {
			t.Fatal("the race's end did not cancel its attempts")
		}

		direct := &quicDialRaceAttempts{schedule: testingDirectRaceSchedule}
		directCtx, _ := direct.launch(t.Context())
		if directCtx.Value(quicDialReceiveClockContextKey{}) != nil {
			t.Fatal("a direct H3 attempt carries a receive clock")
		}
		time.Sleep(time.Hour)
		if next := direct.cancelStalled(time.Now()); !next.IsZero() || directCtx.Err() != nil {
			t.Fatal("a direct H3 race stalled an attempt")
		}
		direct.cancelAll()
	})
}

// One whodis race of each budgeted kind over two candidates: the alt
// dialer's (raceAltQuicDial) and a phone's dns carrier (raceH3DialWithMemory).
// The budget admits claims for at most `claims` attempts.
func runWhodisTestRace(
	ctx context.Context,
	kind string,
	claims int,
	candidates []*net.UDPAddr,
	dial func(context.Context, *net.UDPAddr, *platformTransportBudgetReservation) (*h3DialAttempt, error),
) (*h3DialAttempt, error) {
	const claimBytes = ByteCount(4096)
	switch kind {
	case "alt":
		budget := NewPlatformTransportBudget(claimBytes*ByteCount(claims), claims)
		policy := extenderQuicMemoryPolicy{budget: budget, byteCount: claimBytes, usesSlot: true}
		return raceAltQuicDial(ctx, candidates, policy, testingWhodisRaceSchedule, dial)
	default:
		// the first attempt takes the carrier's base lease, which the budget
		// already holds; each other takes an extra claim
		budget := NewPlatformTransportBudget(claimBytes*ByteCount(claims), claims)
		base := budget.register(platformTransportBudgetH3Explicit, claimBytes, true)
		if !base.TryAcquire() {
			return nil, errors.New("synthetic base admission")
		}
		defer base.Release()
		return raceH3DialWithMemory(ctx, candidates, budget, claimBytes, 0, testingWhodisRaceSchedule,
			func(ctx context.Context, address *net.UDPAddr) (*h3DialAttempt, error) {
				return dial(ctx, address, nil)
			})
	}
}

// One synthetic attempt: its launch time, and whether and why it ended.
type whodisTestAttempt struct {
	launchTime time.Time
	endTime    time.Time
	cause      error
}

type whodisTestAttempts struct {
	stateLock sync.Mutex
	attempts  map[int]*whodisTestAttempt
}

func (self *whodisTestAttempts) launched(port int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.attempts[port] = &whodisTestAttempt{launchTime: time.Now()}
}

func (self *whodisTestAttempts) ended(port int, cause error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.attempts[port].endTime = time.Now()
	self.attempts[port].cause = cause
}

func (self *whodisTestAttempts) get(port int) (whodisTestAttempt, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	attempt, ok := self.attempts[port]
	if !ok {
		return whodisTestAttempt{}, false
	}
	return *attempt, true
}

var whodisTestCandidates = []*net.UDPAddr{{IP: net.ParseIP("192.0.2.53"), Port: 53}, {IP: net.ParseIP("192.0.2.53"), Port: 4053}}

// A whodis attempt on 53 that hears from alt every half second and completes
// its handshake at 3 s, past the stagger and the stall timeout, is never
// ended for 4053 when the budget holds one claim; the race waits for it and
// it wins.
func TestWhodisRaceKeepsAnAttemptHearingFromAlt(t *testing.T) {
	for _, kind := range []string{"alt", "platform"} {
		synctest.Test(t, func(t *testing.T) {
			attempts := &whodisTestAttempts{attempts: map[int]*whodisTestAttempt{}}
			var live atomic.Int32
			dial := func(ctx context.Context, address *net.UDPAddr, claim *platformTransportBudgetReservation) (*h3DialAttempt, error) {
				attempts.launched(address.Port)
				live.Add(1)
				attempt := &h3DialAttempt{
					udpAddr: address, budgetReservation: claim,
					packetConn: &progressiveUdpTestSocket{onClose: func() { live.Add(-1) }},
				}
				clock := ctx.Value(quicDialReceiveClockContextKey{}).(*quicDialReceiveClock)
				heard := time.NewTicker(500 * time.Millisecond)
				defer heard.Stop()
				complete := time.After(3 * time.Second)
				for {
					select {
					case <-heard.C:
						clock.received()
					case <-complete:
						attempts.ended(address.Port, nil)
						return attempt, nil
					case <-ctx.Done():
						attempts.ended(address.Port, context.Cause(ctx))
						attempt.close()
						return nil, context.Cause(ctx)
					}
				}
			}
			winner, err := runWhodisTestRace(t.Context(), kind, 1, whodisTestCandidates, dial)
			if err != nil || winner == nil || winner.udpAddr.Port != 53 {
				t.Fatalf("%s: the hearing attempt did not win: %v %v", kind, winner, err)
			}
			if _, ok := attempts.get(4053); ok {
				t.Fatalf("%s: 4053 launched while 53 heard from alt on the only claim", kind)
			}
			winner.close()
			if live.Load() != 0 {
				t.Fatalf("%s: sockets leaked", kind)
			}
		})
	}
}

// A silent attempt on 53 holding the only claim is ended once it has heard
// nothing for the stall timeout, not at the stagger, with the stall as its
// cause; 4053 then launches on its claim and wins.
func TestWhodisRaceStalledAttemptYieldsTheOnlyClaim(t *testing.T) {
	for _, kind := range []string{"alt", "platform"} {
		synctest.Test(t, func(t *testing.T) {
			attempts := &whodisTestAttempts{attempts: map[int]*whodisTestAttempt{}}
			start := time.Now()
			dial := func(ctx context.Context, address *net.UDPAddr, claim *platformTransportBudgetReservation) (*h3DialAttempt, error) {
				attempts.launched(address.Port)
				attempt := &h3DialAttempt{udpAddr: address, budgetReservation: claim}
				if address.Port == 4053 {
					attempts.ended(address.Port, nil)
					return attempt, nil
				}
				<-ctx.Done()
				attempts.ended(address.Port, context.Cause(ctx))
				attempt.close()
				return nil, context.Cause(ctx)
			}
			winner, err := runWhodisTestRace(t.Context(), kind, 1, whodisTestCandidates, dial)
			if err != nil || winner == nil || winner.udpAddr.Port != 4053 {
				t.Fatalf("%s: the fallback did not win: %v %v", kind, winner, err)
			}
			winner.close()
			first, _ := attempts.get(53)
			fallback, ok := attempts.get(4053)
			if !ok {
				t.Fatalf("%s: 4053 never launched", kind)
			}
			if !errors.Is(first.cause, errQuicDialStalled) {
				t.Fatalf("%s: 53 ended by %v", kind, first.cause)
			}
			if got := first.endTime.Sub(start); got != testingWhodisRaceSchedule.stallTimeout {
				t.Fatalf("%s: 53 ended at %s, want the stall timeout", kind, got)
			}
			if fallback.launchTime.Before(first.endTime) {
				t.Fatalf("%s: 4053 launched before 53 gave up the claim", kind)
			}
		})
	}
}

// With a second claim, 4053 launches beside a silent 53 at the stagger, and
// 53 is not ended for it.
func TestWhodisRaceLaunchesBesideWithASecondClaim(t *testing.T) {
	for _, kind := range []string{"alt", "platform"} {
		synctest.Test(t, func(t *testing.T) {
			attempts := &whodisTestAttempts{attempts: map[int]*whodisTestAttempt{}}
			start := time.Now()
			dial := func(ctx context.Context, address *net.UDPAddr, claim *platformTransportBudgetReservation) (*h3DialAttempt, error) {
				attempts.launched(address.Port)
				attempt := &h3DialAttempt{udpAddr: address, budgetReservation: claim}
				if address.Port == 4053 {
					// 53 must still be out when 4053 completes
					time.Sleep(10 * time.Millisecond)
					if first, _ := attempts.get(53); !first.endTime.IsZero() {
						t.Errorf("%s: 53 ended for the fallback", kind)
					}
					return attempt, nil
				}
				<-ctx.Done()
				attempts.ended(address.Port, context.Cause(ctx))
				attempt.close()
				return nil, context.Cause(ctx)
			}
			winner, err := runWhodisTestRace(t.Context(), kind, 2, whodisTestCandidates, dial)
			if err != nil || winner == nil || winner.udpAddr.Port != 4053 {
				t.Fatalf("%s: the fallback did not win: %v %v", kind, winner, err)
			}
			winner.close()
			fallback, _ := attempts.get(4053)
			if got := fallback.launchTime.Sub(start); got != testingWhodisRaceSchedule.stagger {
				t.Fatalf("%s: 4053 launched at %s, want the stagger", kind, got)
			}
			if first, _ := attempts.get(53); errors.Is(first.cause, errQuicDialStalled) {
				t.Fatalf("%s: 53 was stalled though a claim was free", kind)
			}
		})
	}
}
