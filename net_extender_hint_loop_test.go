package connect

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The hint loop (open bug P052). The hint is read through direct dialers only,
// so where only extenders reach the operator -- a whitelist-only network, a
// blocked api -- every read fails, and a read whose dials are black-holed
// lasts its whole budget. The read runs beside the refresh pass, so no pass
// waits for it, and a failed read is read again only once its backoff has
// passed or the path has changed, so the refresh loop, which in the feed role
// passes after every feed drop, does not dial the operator at each pass.
//
// The network client tests run in a synctest bubble. The hint's budget is a
// real timer, so bubble time advances only while every goroutine waits: a pass
// that waited for a black-holed read takes the read's budget in bubble time,
// and one that did not takes none. When a hint is due is the fake clock's to
// decide.

// An operator the hint cannot reach directly: each read waits until its
// budget ends, as a black-holed dial does.
type testBlackholedExtenderHint struct {
	stateLock sync.Mutex
	count     int
	inFlight  int
}

// The settings seam: each read waits out its budget, as a black-holed dial
// does.
func (self *testBlackholedExtenderHint) Hint(ctx context.Context) (*ExtenderHintResult, error) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.count += 1
		self.inFlight += 1
	}()
	defer func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.inFlight -= 1
	}()
	<-ctx.Done()
	return nil, ctx.Err()
}

// The reads started, and the reads that have not ended.
func (self *testBlackholedExtenderHint) counts() (int, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.count, self.inFlight
}

// A network client whose refresh loop passes only when the test lets it, with
// the hint read through the given seam. Every other seam answers at once, the
// directory is empty and the strategy dials nothing, so a pass that does not
// wait for the hint completes without bubble time advancing.
type testHintLoop struct {
	clock         *testClock
	directory     *ExtenderDirectory
	networkClient *ExtenderNetworkClient
	passes        chan time.Time
	// passes completed: the refresh loop asks for its wait once per pass
	passCount atomic.Int64
}

// The network client of one test, run in its bubble, reading the hint
// through hint; configure, when set, changes the settings before the client
// starts.
func newTestHintLoop(
	t *testing.T,
	hint func(ctx context.Context) (*ExtenderHintResult, error),
	configure func(settings *ExtenderNetworkClientSettings),
) *testHintLoop {
	t.Helper()
	clock := newTestClock()
	directory, _ := newTestExtenderDirectory(t, clock, nil)
	self := &testHintLoop{
		clock:     clock,
		directory: directory,
		passes:    make(chan time.Time),
	}
	settings := DefaultExtenderNetworkClientSettings()
	settings.Log = NewNoopLogger()
	settings.Now = clock.Now
	settings.ExtenderDnsName = "hint-loop.example"
	settings.ProbeWindowCount = 0
	settings.IpVersionSupported = func(ipVersion int) bool { return true }
	settings.PassAfter = func(time.Duration) <-chan time.Time {
		self.passCount.Add(1)
		return self.passes
	}
	settings.Hello = func(context.Context) (*ExtenderHelloResult, error) { return nil, nil }
	settings.Hint = hint
	settings.ResolveDns = func(context.Context, string) ([]netip.Addr, error) { return nil, nil }
	settings.ResolveDnsTxt = func(context.Context, string) ([]string, error) { return nil, nil }
	if configure != nil {
		configure(settings)
	}
	strategy := newTestDeadDialStrategy(t, t.Context())
	self.networkClient = NewExtenderNetworkClient(t.Context(), strategy, directory, settings)
	t.Cleanup(self.networkClient.Close)
	return self
}

// Lets the refresh loop run one more pass and waits for it and the hint loop
// to settle. Returns how long the pass took in bubble time.
func (self *testHintLoop) pass(t *testing.T) time.Duration {
	t.Helper()
	passCount := self.passCount.Load()
	startTime := time.Now()
	self.passes <- time.Time{}
	synctest.Wait()
	if self.passCount.Load() != passCount+1 {
		t.Fatalf("passes = %d, expected the pass to complete (%d)", self.passCount.Load(), passCount+1)
	}
	return time.Since(startTime)
}

// The bug: with the operator black-holed directly, a read ahead of the pass
// held the bootstrap, the manual hosts and the sample of every pass for the
// whole read budget. Now no pass waits for the read -- not the first, not one
// while the read is out, not one after it failed, and not one that starts the
// next read -- and one read is out at a time.
func TestExtenderNetworkClientPassDoesNotWaitForTheHint(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		setTestNetworkCountryCode(t, "ru")
		hint := &testBlackholedExtenderHint{}
		loop := newTestHintLoop(t, hint.Hint, nil)
		settings := DefaultExtenderNetworkClientSettings()

		synctest.Wait()
		if !loop.networkClient.Status().InitialAttemptDone {
			t.Fatal("the first pass waited for the hint before sampling")
		}
		if count, inFlight := hint.counts(); count != 1 || inFlight != 1 {
			t.Fatalf("hint reads = %d, %d out; expected the first read, still out", count, inFlight)
		}
		// no answer yet: the network country the host reports is in force
		if countryCode := loop.directory.SpoofCountryCode(); countryCode != "ru" {
			t.Fatalf("spoof country = %q, expected the network country", countryCode)
		}

		for i := 0; i < 3; i += 1 {
			if elapsed := loop.pass(t); elapsed != 0 {
				t.Fatalf("a pass with the hint out took %s", elapsed)
			}
		}
		if count, _ := hint.counts(); count != 1 {
			t.Fatalf("hint reads = %d, expected no second read while the first is out", count)
		}

		// the read runs out its budget beside the parked refresh loop
		time.Sleep(settings.HelloTimeout + time.Second)
		synctest.Wait()
		if count, inFlight := hint.counts(); count != 1 || inFlight != 0 {
			t.Fatalf("hint reads = %d, %d out; expected the first read to have failed", count, inFlight)
		}
		if countryCode := loop.directory.SpoofCountryCode(); countryCode != "ru" {
			t.Fatalf("spoof country = %q, expected the network country after the failure", countryCode)
		}

		// after the failure: a pass in the backoff reads nothing, and the
		// pass that starts the next read does not wait for it either
		if elapsed := loop.pass(t); elapsed != 0 {
			t.Fatalf("a pass after the failure took %s", elapsed)
		}
		if count, _ := hint.counts(); count != 1 {
			t.Fatalf("hint reads = %d, expected none in the failure's backoff", count)
		}
		loop.clock.advance(settings.HintMinBackoff)
		if elapsed := loop.pass(t); elapsed != 0 {
			t.Fatalf("the pass that started the next read took %s", elapsed)
		}
		if count, inFlight := hint.counts(); count != 2 || inFlight != 1 {
			t.Fatalf("hint reads = %d, %d out; expected the second read, out", count, inFlight)
		}

		// closing ends the read that is out and joins the hint loop
		loop.networkClient.Close()
		select {
		case <-loop.networkClient.hintDone:
		default:
			t.Fatal("Close returned before joining the hint loop")
		}
		if _, inFlight := hint.counts(); inFlight != 0 {
			t.Fatal("Close left a hint read out")
		}
	})
}

// After a failure no pass reads the hint again until the backoff has passed,
// and each further failure on the path doubles the backoff, up to the max.
// Meanwhile the network country stands in. When the operator answers again,
// the first read after the backoff takes its country and continent, and an
// answer is read again only on the refresh period.
func TestExtenderNetworkClientReadsAFailedHintAgainAfterItsBackoff(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		setTestNetworkCountryCode(t, "ru")
		hint := newTestExtenderHint(&ExtenderHintResult{})
		hint.Fail(errors.New("no operator address is routable directly on this path"))
		loop := newTestHintLoop(t, hint.Hint, nil)
		settings := DefaultExtenderNetworkClientSettings()

		synctest.Wait()
		if count := hint.Count(); count != 1 {
			t.Fatalf("hint reads = %d, expected the first", count)
		}
		for i := 0; i < 5; i += 1 {
			loop.pass(t)
		}
		if count := hint.Count(); count != 1 {
			t.Fatalf("hint reads = %d after 5 passes, expected none in the backoff", count)
		}
		if countryCode := loop.directory.SpoofCountryCode(); countryCode != "ru" {
			t.Fatalf("spoof country = %q, expected the network country", countryCode)
		}

		// each failure doubles the wait before the next read
		count := hint.Count()
		backoff := settings.HintMinBackoff
		for backoff < settings.HintMaxBackoff {
			loop.clock.advance(backoff - time.Second)
			loop.pass(t)
			if hint.Count() != count {
				t.Fatalf("hint reads = %d before the %s backoff passed, expected %d", hint.Count(), backoff, count)
			}
			loop.clock.advance(time.Second)
			loop.pass(t)
			count += 1
			if hint.Count() != count {
				t.Fatalf("hint reads = %d once the %s backoff passed, expected %d", hint.Count(), backoff, count)
			}
			backoff *= 2
		}
		// and never waits longer than the max
		for i := 0; i < 2; i += 1 {
			loop.clock.advance(settings.HintMaxBackoff)
			loop.pass(t)
			count += 1
			if hint.Count() != count {
				t.Fatalf("hint reads = %d after the max backoff, expected %d", hint.Count(), count)
			}
		}
		if countryCode := loop.directory.SpoofCountryCode(); countryCode != "ru" {
			t.Fatalf("spoof country = %q, expected the network country", countryCode)
		}

		// the direct path recovers: the first read after the backoff answers
		hint.Answer(&ExtenderHintResult{ContinentCode: "EU", CountryCode: "de"})
		loop.pass(t)
		if hint.Count() != count {
			t.Fatalf("hint reads = %d before the backoff passed, expected %d", hint.Count(), count)
		}
		loop.clock.advance(settings.HintMaxBackoff)
		loop.pass(t)
		count += 1
		if hint.Count() != count {
			t.Fatalf("hint reads = %d once the backoff passed, expected %d", hint.Count(), count)
		}
		if countryCode := loop.directory.SpoofCountryCode(); countryCode != "de" {
			t.Fatalf("spoof country = %q, expected the operator's", countryCode)
		}
		if continentCode := loop.directory.ContinentHint(); continentCode != "EU" {
			t.Fatalf("continent = %q, expected the operator's", continentCode)
		}

		// an answer holds for the refresh period
		loop.clock.advance(settings.RebootstrapTimeout - time.Second)
		loop.pass(t)
		if hint.Count() != count {
			t.Fatalf("hint reads = %d within the refresh period, expected %d", hint.Count(), count)
		}
		loop.clock.advance(time.Second)
		loop.pass(t)
		if hint.Count() != count+1 {
			t.Fatalf("hint reads = %d after the refresh period, expected %d", hint.Count(), count+1)
		}
	})
}

// A path change reads a failed hint again at once, whatever backoff the
// failures on the old path had built up, and a failure on the new path backs
// off from the minimum. A path where the operator answers gets the hint.
func TestExtenderNetworkClientReadsAFailedHintAgainAfterAPathChange(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		setTestNetworkCountryCode(t, "ru")
		hint := newTestExtenderHint(&ExtenderHintResult{})
		hint.Fail(errors.New("no operator address is routable directly on this path"))
		loop := newTestHintLoop(t, hint.Hint, nil)
		settings := DefaultExtenderNetworkClientSettings()

		synctest.Wait()
		// three failures on this path: the next waits four times the minimum
		loop.clock.advance(settings.HintMinBackoff)
		loop.pass(t)
		loop.clock.advance(2 * settings.HintMinBackoff)
		loop.pass(t)
		if count := hint.Count(); count != 3 {
			t.Fatalf("hint reads = %d, expected 3", count)
		}
		loop.pass(t)
		if count := hint.Count(); count != 3 {
			t.Fatalf("hint reads = %d, expected none in the backoff", count)
		}

		// the clock has not moved: only the path change reads it
		loop.networkClient.networkChanged()
		synctest.Wait()
		if count := hint.Count(); count != 4 {
			t.Fatalf("hint reads = %d after the path change, expected 4", count)
		}
		if countryCode := loop.directory.SpoofCountryCode(); countryCode != "ru" {
			t.Fatalf("spoof country = %q, expected the network country", countryCode)
		}
		// the failure on the new path waits the minimum, not eight times it
		loop.pass(t)
		if count := hint.Count(); count != 4 {
			t.Fatalf("hint reads = %d, expected none in the new path's backoff", count)
		}
		loop.clock.advance(settings.HintMinBackoff)
		loop.pass(t)
		if count := hint.Count(); count != 5 {
			t.Fatalf("hint reads = %d once the minimum backoff passed, expected 5", count)
		}

		// a path where the operator answers directly gets the hint at once
		hint.Answer(&ExtenderHintResult{ContinentCode: "EU", CountryCode: "de"})
		loop.networkClient.networkChanged()
		synctest.Wait()
		if count := hint.Count(); count != 6 {
			t.Fatalf("hint reads = %d after the second path change, expected 6", count)
		}
		if countryCode := loop.directory.SpoofCountryCode(); countryCode != "de" {
			t.Fatalf("spoof country = %q, expected the operator's", countryCode)
		}
		if continentCode := loop.directory.ContinentHint(); continentCode != "EU" {
			t.Fatalf("continent = %q, expected the operator's", continentCode)
		}
	})
}

// A path change wakes the hint loop itself, so the read does not wait for a
// pass the refresh loop is still busy with: here a bootstrap the resolver
// holds.
func TestExtenderNetworkClientPathChangeReadsTheHintWhileAPassIsBusy(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		hint := newTestExtenderHint(&ExtenderHintResult{})
		hint.Fail(errors.New("no operator address is routable directly on this path"))
		var holdBootstrap atomic.Bool
		bootstrapHeld := make(chan struct{})
		releaseBootstrap := make(chan struct{})
		loop := newTestHintLoop(t, hint.Hint, func(settings *ExtenderNetworkClientSettings) {
			settings.ResolveDnsTxt = func(ctx context.Context, _ string) ([]string, error) {
				if holdBootstrap.Load() {
					close(bootstrapHeld)
					select {
					case <-releaseBootstrap:
					case <-ctx.Done():
					}
				}
				return nil, nil
			}
		})

		synctest.Wait()
		holdBootstrap.Store(true)
		loop.passes <- time.Time{}
		<-bootstrapHeld
		synctest.Wait()
		if count := hint.Count(); count != 1 {
			t.Fatalf("hint reads = %d, expected none in the backoff", count)
		}

		loop.networkClient.networkChanged()
		synctest.Wait()
		if count := hint.Count(); count != 2 {
			t.Fatalf("hint reads = %d after the path change, expected the read while the pass is held", count)
		}

		holdBootstrap.Store(false)
		close(releaseBootstrap)
		synctest.Wait()
	})
}

// The first hint read runs beside the bootstrap, and the first probe pass
// waits for it, so the operator's continent is probed first: the bootstrap
// alone (a split answer) implies no continent, and probing then would spend
// pings on the other one.
func TestExtenderNetworkClientFirstProbePassWaitsForTheHint(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		clock := newTestClock()
		directory, rootPrivateKey := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
			settings.HoldTimeout = 0
			settings.MaxHoldTimeout = 0
		})
		txts := []string{
			testExtenderDnsRecordTxtWithContinent(t, rootPrivateKey, clock, "NA", "192.0.2.20"),
			testExtenderDnsRecordTxtWithContinent(t, rootPrivateKey, clock, "NA", "192.0.2.21"),
			testExtenderDnsRecordTxtWithContinent(t, rootPrivateKey, clock, "EU", "192.0.2.10"),
			testExtenderDnsRecordTxtWithContinent(t, rootPrivateKey, clock, "EU", "192.0.2.11"),
		}
		probes := newTestProbeLog(map[string]time.Duration{
			"192.0.2.10": 20 * time.Millisecond,
			"192.0.2.11": 25 * time.Millisecond,
			"192.0.2.20": 120 * time.Millisecond,
			"192.0.2.21": 130 * time.Millisecond,
		})
		releaseHint := make(chan struct{})
		client := newTestExtenderStartupProbeClient(t, clock, directory, probes, func(settings *ExtenderNetworkClientSettings) {
			settings.ResolveDnsTxt = func(context.Context, string) ([]string, error) {
				return txts, nil
			}
			settings.Hint = func(ctx context.Context) (*ExtenderHintResult, error) {
				select {
				case <-releaseHint:
					return &ExtenderHintResult{ContinentCode: "eu"}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		})

		synctest.Wait()
		if !client.Status().InitialAttemptDone {
			t.Fatal("the first pass waited for the hint")
		}
		if count := probes.count(); count != 0 {
			t.Fatalf("%d probes ran before the first hint read ended", count)
		}

		close(releaseHint)
		synctest.Wait()
		ips := func() []string {
			probes.stateLock.Lock()
			defer probes.stateLock.Unlock()
			return slices.Clone(probes.ips)
		}()
		slices.Sort(ips)
		if !slices.Equal(ips, []string{"192.0.2.10", "192.0.2.11"}) {
			t.Fatalf("probes = %v, expected exactly the two extenders on the operator's continent", ips)
		}
	})
}

// The schedule alone: due at a start and after a path change; an answer
// holds for the refresh timeout; a failure waits the minimum backoff, which
// doubles with each further failure up to the max, and an answer or a path
// change starts it over. A max below the minimum is the minimum.
func TestExtenderHintScheduleBacksOffAFailure(t *testing.T) {
	refreshTimeout := 6 * time.Hour
	minBackoff := time.Minute
	maxBackoff := 5 * time.Minute
	hintSchedule := newExtenderReadSchedule(refreshTimeout, minBackoff, maxBackoff)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	assertNextDue := func(wait time.Duration) {
		t.Helper()
		if hintSchedule.Due(now.Add(wait - time.Nanosecond)) {
			t.Fatalf("due before %s", wait)
		}
		if !hintSchedule.Due(now.Add(wait)) {
			t.Fatalf("not due after %s", wait)
		}
	}

	if !hintSchedule.Due(now) {
		t.Fatal("not due at a start")
	}
	for _, backoff := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		hintSchedule.Fail(now)
		assertNextDue(backoff)
		now = now.Add(backoff)
	}

	hintSchedule.Answer(now)
	assertNextDue(refreshTimeout)
	hintSchedule.Fail(now)
	assertNextDue(minBackoff)

	hintSchedule.Fail(now)
	assertNextDue(2 * minBackoff)
	hintSchedule.Rearm()
	if !hintSchedule.Due(now) {
		t.Fatal("not due after a path change")
	}
	hintSchedule.Fail(now)
	assertNextDue(minBackoff)

	hintSchedule = newExtenderReadSchedule(refreshTimeout, minBackoff, 0)
	hintSchedule.Fail(now)
	hintSchedule.Fail(now)
	assertNextDue(minBackoff)
}

// The operator's continent outranks the dns inference however the two
// interleave around the directory, which is set outside continentLock. Here
// the inference has read its decision when the operator answers, so the
// inference sets the directory after the operator has: it then finds a newer
// decision and sets that, and the directory and the status end at the
// operator's continent. An inference decided after the operator's answer is
// refused. A setter that did not check for a newer decision would leave the
// inference's continent in force.
func TestExtenderNetworkClientContinentHintKeepsTheOperatorsOverALateInference(t *testing.T) {
	clock := newTestClock()
	directory, _ := newTestExtenderDirectory(t, clock, nil)
	networkClient := newTestBareExtenderNetworkClient(t, directory)
	// the operator answers once, in the inference's window between reading
	// its decision and setting the directory
	operatorAnswered := false
	networkClient.continentHintSetHook = func() {
		if operatorAnswered {
			return
		}
		operatorAnswered = true
		networkClient.setContinentHint("EU", extenderContinentHintSourceOperator)
	}
	networkClient.setContinentHint("NA", extenderContinentHintSourceDns)
	if !operatorAnswered {
		t.Fatal("the operator did not answer in the inference's window")
	}
	if continentHint := directory.ContinentHint(); continentHint != "EU" {
		t.Fatalf("directory hint = %q, expected the operator's EU over the inference set after it", continentHint)
	}
	if continentHint := networkClient.Status().ContinentHint; continentHint != "EU" {
		t.Fatalf("status hint = %q, expected the operator's EU over the inference set after it", continentHint)
	}

	networkClient.continentHintSetHook = nil
	networkClient.setContinentHint("AS", extenderContinentHintSourceDns)
	if continentHint := directory.ContinentHint(); continentHint != "EU" {
		t.Fatalf("directory hint = %q, expected an inference after the operator's answer refused", continentHint)
	}
	if continentHint := networkClient.Status().ContinentHint; continentHint != "EU" {
		t.Fatalf("status hint = %q, expected an inference after the operator's answer refused", continentHint)
	}
}
