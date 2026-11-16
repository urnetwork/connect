// IPV6.md C7: a named first family through the progressive DoH dial race.
// Answers are fed directly or from a pre-filled cache; dials are in-memory
// pipes, and virtual time measures every wait exactly.
package connect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

var firstFamilyAddrs = map[int]netip.Addr{
	4: netip.MustParseAddr("192.0.2.95"),
	6: netip.MustParseAddr("2001:db8::95"),
}

func otherIpFamily(family int) int {
	if family == 4 {
		return 6
	}
	return 4
}

func addrIpFamily(addr netip.Addr) int {
	if addr.Unmap().Is4() {
		return 4
	}
	return 6
}

// One record type's answer, marked as dialDohAddrsRace marks it.
func firstFamilyAnswer(family int, addrs ...netip.Addr) dohDialQueryResult {
	return dohDialQueryResult{addrs: addrs, authoritative: true, ipv6: family == 6}
}

// One dial start: the address and the virtual time since the race began.
type firstFamilyLaunch struct {
	addr netip.Addr
	at   time.Duration
}

// A connected pipe that remembers the address it was dialed for.
type firstFamilyConn struct {
	net.Conn
	addr netip.Addr
}

// Records every launch; failing families error at once and hanging families
// block until the race cancels them. Everything else connects.
type firstFamilyDialer struct {
	started   time.Time
	stateLock sync.Mutex
	launches  []firstFamilyLaunch
	fail      map[int]bool
	hang      map[int]bool
}

func newFirstFamilyDialer() *firstFamilyDialer {
	return &firstFamilyDialer{started: time.Now(), fail: map[int]bool{}, hang: map[int]bool{}}
}

func (self *firstFamilyDialer) dial(ctx context.Context, addr netip.Addr) (net.Conn, error) {
	self.stateLock.Lock()
	self.launches = append(self.launches, firstFamilyLaunch{addr: addr, at: time.Since(self.started)})
	self.stateLock.Unlock()
	family := addrIpFamily(addr)
	if self.fail[family] {
		return nil, fmt.Errorf("ipv%d refused", family)
	}
	if self.hang[family] {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	client, peer := net.Pipe()
	_ = peer.Close()
	return &firstFamilyConn{Conn: client, addr: addr}, nil
}

func (self *firstFamilyDialer) recorded() []firstFamilyLaunch {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]firstFamilyLaunch(nil), self.launches...)
}

// Runs one progressive race over the given answers, of which pending are
// expected; an answer never sent stays pending until the race returns. A
// race that never launches fails at its deadline instead of hanging.
func raceFirstFamily(dialer *firstFamilyDialer, firstIpFamily int, fallbackDelay time.Duration, pending int, answers ...dohDialQueryResult) (netip.Addr, error) {
	results := make(chan dohDialQueryResult, len(answers))
	for _, answer := range answers {
		results <- answer
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, err := dialAddrsRaceWithResolution(ctx, nil, fallbackDelay, dialer.dial, &dialAddrResolution{
		results:       results,
		pending:       pending,
		host:          "first-family.example",
		firstIpFamily: firstIpFamily,
	})
	if err != nil {
		return netip.Addr{}, err
	}
	defer conn.Close()
	return conn.(*firstFamilyConn).addr, nil
}

// IPV6.md C7. The first family is attempted first even when the other
// family's answer was published first; the other never starts while the
// first connects.
func TestDialFirstIpFamilyLeadsWhenOtherAnswersFirst(t *testing.T) {
	for _, first := range []int{4, 6} {
		other := otherIpFamily(first)
		dialer := newFirstFamilyDialer()
		landed, err := raceFirstFamily(dialer, first, time.Hour, 2,
			firstFamilyAnswer(other, firstFamilyAddrs[other]),
			firstFamilyAnswer(first, firstFamilyAddrs[first]),
		)
		if err != nil {
			t.Fatalf("first v%d: %v", first, err)
		}
		if launches := dialer.recorded(); len(launches) != 1 || landed != firstFamilyAddrs[first] {
			t.Fatalf("first v%d: launches=%v landed=%s, want only %s", first, launches, landed, firstFamilyAddrs[first])
		}
	}
}

// IPV6.md C7. While the first family's answer is pending, a ready answer of
// the other family waits exactly one fallback delay and then starts.
func TestDialFirstIpFamilyWaitIsBoundedByFallbackDelay(t *testing.T) {
	for _, first := range []int{4, 6} {
		synctest.Test(t, func(t *testing.T) {
			other := otherIpFamily(first)
			dialer := newFirstFamilyDialer()
			landed, err := raceFirstFamily(dialer, first, DefaultDialFallbackDelay, 2,
				firstFamilyAnswer(other, firstFamilyAddrs[other]),
			)
			if err != nil {
				t.Fatalf("first v%d: %v", first, err)
			}
			launches := dialer.recorded()
			if len(launches) != 1 || landed != firstFamilyAddrs[other] || launches[0].at != DefaultDialFallbackDelay {
				t.Fatalf("first v%d: launches=%v, want %s at %s", first, launches, firstFamilyAddrs[other], DefaultDialFallbackDelay)
			}
		})
	}
}

// IPV6.md C7. A first family with no usable answer (authoritatively empty or
// failed) releases the other family at once, in either publication order.
func TestDialFirstIpFamilyMissingAnswerStartsOtherAtOnce(t *testing.T) {
	for _, first := range []int{4, 6} {
		other := otherIpFamily(first)
		failed := firstFamilyAnswer(first)
		failed.authoritative = false
		for name, answers := range map[string][]dohDialQueryResult{
			"empty after":  {firstFamilyAnswer(other, firstFamilyAddrs[other]), firstFamilyAnswer(first)},
			"empty before": {firstFamilyAnswer(first), firstFamilyAnswer(other, firstFamilyAddrs[other])},
			"failed after": {firstFamilyAnswer(other, firstFamilyAddrs[other]), failed},
		} {
			synctest.Test(t, func(t *testing.T) {
				dialer := newFirstFamilyDialer()
				landed, err := raceFirstFamily(dialer, first, DefaultDialFallbackDelay, 2, answers...)
				if err != nil {
					t.Fatalf("first v%d %s: %v", first, name, err)
				}
				launches := dialer.recorded()
				if len(launches) != 1 || landed != firstFamilyAddrs[other] || launches[0].at != 0 {
					t.Fatalf("first v%d %s: launches=%v, want %s at once", first, name, launches, firstFamilyAddrs[other])
				}
			})
		}
	}
}

// IPV6.md C7. A definitive failure of the first family starts the other at
// once, and the other family's connection is used.
func TestDialFirstIpFamilyFallsBackAtOnceWhenFirstFails(t *testing.T) {
	for _, first := range []int{4, 6} {
		synctest.Test(t, func(t *testing.T) {
			other := otherIpFamily(first)
			dialer := newFirstFamilyDialer()
			dialer.fail[first] = true
			landed, err := raceFirstFamily(dialer, first, DefaultDialFallbackDelay, 2,
				firstFamilyAnswer(other, firstFamilyAddrs[other]),
				firstFamilyAnswer(first, firstFamilyAddrs[first]),
			)
			if err != nil {
				t.Fatalf("first v%d: %v", first, err)
			}
			launches := dialer.recorded()
			if len(launches) != 2 || launches[0].addr != firstFamilyAddrs[first] || landed != firstFamilyAddrs[other] || launches[1].at != 0 {
				t.Fatalf("first v%d: launches=%v, want %s then %s at once", first, launches, firstFamilyAddrs[first], firstFamilyAddrs[other])
			}
		})
	}
}

// IPV6.md C7. A first family that never answers its connect is overtaken by
// the other family one fallback delay after it started.
func TestDialFirstIpFamilyFallsBackAfterDelayWhenFirstHangs(t *testing.T) {
	for _, first := range []int{4, 6} {
		synctest.Test(t, func(t *testing.T) {
			other := otherIpFamily(first)
			dialer := newFirstFamilyDialer()
			dialer.hang[first] = true
			landed, err := raceFirstFamily(dialer, first, DefaultDialFallbackDelay, 2,
				firstFamilyAnswer(first, firstFamilyAddrs[first]),
				firstFamilyAnswer(other, firstFamilyAddrs[other]),
			)
			if err != nil {
				t.Fatalf("first v%d: %v", first, err)
			}
			launches := dialer.recorded()
			if len(launches) != 2 || launches[0].addr != firstFamilyAddrs[first] || launches[0].at != 0 ||
				landed != firstFamilyAddrs[other] || launches[1].at != DefaultDialFallbackDelay {
				t.Fatalf("first v%d: launches=%v, want %s at once then %s after %s", first, launches, firstFamilyAddrs[first], firstFamilyAddrs[other], DefaultDialFallbackDelay)
			}
		})
	}
}

// IPV6.md C6, C7. No first family (0, or a value that is neither 4 nor 6)
// keeps the shared rule: the first usable answer starts at once.
func TestDialFirstIpFamilyUnsetKeepsFirstAnswerRule(t *testing.T) {
	for _, firstIpFamily := range []int{0, 5} {
		for _, ready := range []int{4, 6} {
			synctest.Test(t, func(t *testing.T) {
				dialer := newFirstFamilyDialer()
				landed, err := raceFirstFamily(dialer, firstIpFamily, DefaultDialFallbackDelay, 2,
					firstFamilyAnswer(ready, firstFamilyAddrs[ready]),
				)
				if err != nil {
					t.Fatalf("first %d ready v%d: %v", firstIpFamily, ready, err)
				}
				launches := dialer.recorded()
				if len(launches) != 1 || landed != firstFamilyAddrs[ready] || launches[0].at != 0 {
					t.Fatalf("first %d ready v%d: launches=%v, want %s at once", firstIpFamily, ready, launches, firstFamilyAddrs[ready])
				}
			})
		}
	}
}

// A cache whose ready record types answer at once and whose held types join
// a flight that never finishes. No resolver is configured.
func newFirstFamilyCache(host string, ready map[string]netip.Addr, held ...string) *DohCache {
	settings := DefaultDohSettings()
	settings.Log = NewNoopLogger()
	settings.RequestTimeout = time.Hour
	settings.DnsResolverSettings = &DnsResolverSettings{}
	cache := NewDohCache(settings)
	for recordType, addr := range ready {
		cache.queryResultExpiration[NewDohKey(recordType, host)] = &DohResult{
			Time:            time.Now(),
			AddrExpirations: map[netip.Addr]time.Time{addr: time.Now().Add(time.Hour)},
		}
	}
	for _, recordType := range held {
		cache.inflight[NewDohKey(recordType, host)] = &dohFlight{done: make(chan struct{})}
	}
	return cache
}

// IPV6.md C7. Through the resolver entry point: with both answers ready the
// first family is the only dial; with the first family's answer held, the
// other family starts exactly one fallback delay later. This also proves the
// entry point marks each answer's family.
func TestDohDialFirstIpFamilyThroughResolver(t *testing.T) {
	recordTypes := map[int]string{4: "A", 6: "AAAA"}
	for _, first := range []int{4, 6} {
		other := otherIpFamily(first)
		for _, held := range []bool{false, true} {
			synctest.Test(t, func(t *testing.T) {
				ready := map[string]netip.Addr{recordTypes[other]: firstFamilyAddrs[other]}
				var heldTypes []string
				if held {
					heldTypes = []string{recordTypes[first]}
				} else {
					ready[recordTypes[first]] = firstFamilyAddrs[first]
				}
				cache := newFirstFamilyCache("first-family.example", ready, heldTypes...)
				defer cache.Close()
				dialer := newFirstFamilyDialer()
				conn, err := dialDohAddrsRace(context.Background(), cache, "tcp", "first-family.example", first, DefaultDialFallbackDelay, dialer.dial)
				if err != nil {
					t.Fatalf("first v%d held=%t: %v", first, held, err)
				}
				defer conn.Close()
				want := firstFamilyLaunch{addr: firstFamilyAddrs[first]}
				if held {
					want = firstFamilyLaunch{addr: firstFamilyAddrs[other], at: DefaultDialFallbackDelay}
				}
				if launches := dialer.recorded(); len(launches) != 1 || launches[0] != want || conn.(*firstFamilyConn).addr != want.addr {
					t.Fatalf("first v%d held=%t: launches=%v, want [%v]", first, held, launches, want)
				}
			})
		}
	}
}

// IPV6.md C7. A network narrowed to one family keeps it: the other family is
// neither queried nor waited for, whatever first family is named.
func TestDohDialFirstIpFamilyKeepsNarrowedNetwork(t *testing.T) {
	for network, family := range map[string]int{"tcp4": 4, "tcp6": 6} {
		synctest.Test(t, func(t *testing.T) {
			recordType := "A"
			if family == 6 {
				recordType = "AAAA"
			}
			// A wait for the absent family would show as a late dial.
			cache := newFirstFamilyCache("narrowed.example", map[string]netip.Addr{recordType: firstFamilyAddrs[family]})
			defer cache.Close()
			dialer := newFirstFamilyDialer()
			conn, err := dialDohAddrsRace(context.Background(), cache, network, "narrowed.example", otherIpFamily(family), DefaultDialFallbackDelay, dialer.dial)
			if err != nil {
				t.Fatalf("%s: %v", network, err)
			}
			defer conn.Close()
			want := firstFamilyLaunch{addr: firstFamilyAddrs[family]}
			if launches := dialer.recorded(); len(launches) != 1 || launches[0] != want {
				t.Fatalf("%s first v%d: launches=%v, want [%v]", network, otherIpFamily(family), launches, want)
			}
		})
	}
}

// The race's own error for a family-less resolution stays unchanged with a
// first family: no answer at all is a resolution error, never a dial.
func TestDialFirstIpFamilyNoAnswersIsResolutionError(t *testing.T) {
	for _, first := range []int{4, 6} {
		dialer := newFirstFamilyDialer()
		_, err := raceFirstFamily(dialer, first, DefaultDialFallbackDelay, 2,
			firstFamilyAnswer(first), firstFamilyAnswer(otherIpFamily(first)),
		)
		var dnsErr *net.DNSError
		if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound || len(dialer.recorded()) != 0 {
			t.Fatalf("first v%d: err=%v launches=%v, want not found and no dial", first, err, dialer.recorded())
		}
	}
}
