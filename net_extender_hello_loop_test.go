package connect

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The hello loop (open bug P052). Hello, which carries the root keys, reads
// through the client strategy, so where only extenders reach the operator --
// a whitelist-only network, a blocked api -- it fails until the bootstrap has
// found one, and a read whose dials are black-holed lasts its whole budget.
// The read runs beside the refresh pass, so no pass waits for it, and a failed
// read is read again only once its backoff has passed or the path has changed.
// The keys still verify everything the pass applies: a pass verifies under the
// keys in force, the bootstrap's TXT records wait for keys where none are in
// force, and keys that hello installs judge the directory, the bootstrap, the
// pass's candidates and the sample again. Since hello now installs keys while
// a pass applies, the directory judges a message again when the keys it was
// verified under were replaced before it was stored.
//
// The network client tests run in a synctest bubble, as the hint loop's do:
// hello's budget is a real timer, so a pass that waited for a black-holed read
// takes the read's budget in bubble time, and one that did not takes none.
// When hello is due is the fake clock's to decide.

// A hello the test answers. A read takes the answer or the error in force when
// it starts; a black-holed read waits until its budget ends, and a held one
// until the test releases it, then answers with what is in force then.
type testExtenderHello struct {
	stateLock  sync.Mutex
	result     *ExtenderHelloResult
	err        error
	blackholed bool
	held       chan struct{}
	count      int
	inFlight   int
}

// A hello that answers with the given result until told otherwise.
func newTestExtenderHello(result *ExtenderHelloResult) *testExtenderHello {
	return &testExtenderHello{
		result: result,
	}
}

// The settings seam.
func (self *testExtenderHello) Hello(ctx context.Context) (*ExtenderHelloResult, error) {
	blackholed, held := func() (bool, chan struct{}) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.count += 1
		self.inFlight += 1
		return self.blackholed, self.held
	}()
	defer func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.inFlight -= 1
	}()
	if blackholed {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if held != nil {
		select {
		case <-held:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return func() (*ExtenderHelloResult, error) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.err != nil {
			return nil, self.err
		}
		result := *self.result
		return &result, nil
	}()
}

// The operator answers with this from the next read on.
func (self *testExtenderHello) Answer(result *ExtenderHelloResult) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.result = result
	self.err = nil
	self.blackholed = false
}

// The operator cannot be asked from the next read on.
func (self *testExtenderHello) Fail(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.err = err
}

// The operator's address is black-holed from the next read on.
func (self *testExtenderHello) Blackhole() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.blackholed = true
}

// The reads from the next on wait until the returned release.
func (self *testExtenderHello) Hold() func() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	held := make(chan struct{})
	self.held = held
	return func() {
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.held == held {
				self.held = nil
			}
		}()
		close(held)
	}
}

// The reads started, and the reads that have not ended.
func (self *testExtenderHello) counts() (int, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.count, self.inFlight
}

// The answer of a hello that carries these root keys.
func testHelloResultWithRootKeys(rootPublicKeys ...ed25519.PublicKey) *ExtenderHelloResult {
	rootPublicKeyHexes := []string{}
	for _, rootPublicKey := range rootPublicKeys {
		rootPublicKeyHexes = append(rootPublicKeyHexes, ExtenderKeySeedHex(rootPublicKey))
	}
	return &ExtenderHelloResult{
		RootPublicKeyHexes: rootPublicKeyHexes,
	}
}

// A directory with no root keys in force: a space with no configured or
// bundled keys, before its first hello.
func newTestUnanchoredExtenderDirectory(
	t *testing.T,
	clock *testClock,
	configure func(settings *ExtenderDirectorySettings),
) *ExtenderDirectory {
	t.Helper()
	settings := DefaultExtenderDirectorySettings()
	settings.Now = clock.Now
	settings.NetworkHosts = []string{testExtenderNetworkHost}
	if configure != nil {
		configure(settings)
	}
	ctx, cancel := context.WithCancel(context.Background())
	directory := NewExtenderDirectory(ctx, settings)
	t.Cleanup(func() {
		directory.Close()
		cancel()
	})
	return directory
}

// A network client built as a bare struct, with none of its loops running, as
// a test builds one to drive a step by hand. It has the fields every client
// has had since before the hello loop, and none of that loop's own.
func newTestBareExtenderNetworkClient(t *testing.T, directory *ExtenderDirectory) *ExtenderNetworkClient {
	t.Helper()
	return &ExtenderNetworkClient{
		ctx:           t.Context(),
		log:           NewNoopLogger(),
		directory:     directory,
		settings:      DefaultExtenderNetworkClientSettings(),
		statusMonitor: NewMonitorValue[ExtenderNetworkClientStatus](ExtenderNetworkClientStatus{}),
		wakeMonitor:   NewMonitor(),
		probeWake:     NewMonitor(),
		hintWake:      NewMonitor(),
	}
}

// A network client whose refresh loop passes only when the test lets it, with
// hello read through the given seam and the bootstrap's TXT answer the given
// records. Every other seam answers at once and the host has no family to
// dial, so a pass that does not wait for hello completes without bubble time
// advancing. The low-water mark is off: the bootstrap repeats only when it is
// due for another reason.
type testHelloLoop struct {
	clock         *testClock
	directory     *ExtenderDirectory
	networkClient *ExtenderNetworkClient
	passes        chan time.Time
	// passes completed: the refresh loop asks for its wait once per pass
	passCount atomic.Int64
	// TXT resolutions, one per bootstrap that judged records
	txtCount atomic.Int64
}

// The network client of a hello loop test over the given directory, closed
// with the test.
func newTestHelloLoop(
	t *testing.T,
	clock *testClock,
	directory *ExtenderDirectory,
	hello func(ctx context.Context) (*ExtenderHelloResult, error),
	txts []string,
	configure func(settings *ExtenderNetworkClientSettings),
) *testHelloLoop {
	t.Helper()
	self := &testHelloLoop{
		clock:     clock,
		directory: directory,
		passes:    make(chan time.Time),
	}
	settings := DefaultExtenderNetworkClientSettings()
	settings.Log = NewNoopLogger()
	settings.Now = clock.Now
	settings.ExtenderDnsName = "hello-loop.example"
	settings.ProbeWindowCount = 0
	settings.LowWaterCount = 0
	settings.IpVersionSupported = func(ipVersion int) bool { return false }
	settings.PassAfter = func(time.Duration) <-chan time.Time {
		self.passCount.Add(1)
		return self.passes
	}
	settings.Hello = hello
	settings.Hint = func(context.Context) (*ExtenderHintResult, error) { return &ExtenderHintResult{}, nil }
	settings.ResolveDns = func(context.Context, string) ([]netip.Addr, error) { return nil, nil }
	settings.ResolveDnsTxt = func(context.Context, string) ([]string, error) {
		self.txtCount.Add(1)
		return txts, nil
	}
	if configure != nil {
		configure(settings)
	}
	strategy := newTestDeadDialStrategy(t, t.Context())
	self.networkClient = NewExtenderNetworkClient(t.Context(), strategy, directory, settings)
	t.Cleanup(self.networkClient.Close)
	return self
}

// Lets the refresh loop run one more pass and waits for it and the loops
// beside it to settle. Returns how long the pass took in bubble time.
func (self *testHelloLoop) pass(t *testing.T) time.Duration {
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

// Whether the address is in the directory with a verified key.
func testDirectoryAddressVerified(directory *ExtenderDirectory, ip string) bool {
	for _, entry := range directory.Snapshot().Entries {
		if entry.Ip == netip.MustParseAddr(ip) {
			return 0 < len(entry.PublicKey)
		}
	}
	return false
}

// The bug: with the operator black-holed, hello at the head of the pass held
// the bootstrap, the manual hosts and the sample of every pass for the whole
// read budget, and a failed read was read again at every pass. Now no pass
// waits for hello -- not the first, not one while the read is out, not one
// after it failed, and not one that starts the next read -- and one read is
// out at a time. Meanwhile the pass verifies under the keys in force.
func TestExtenderNetworkClientPassDoesNotWaitForTheHello(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		clock := newTestClock()
		directory, rootPrivateKey := newTestExtenderDirectory(t, clock, nil)
		txt := testExtenderDnsRecordTxt(t, rootPrivateKey, clock, "192.0.2.80")
		hello := newTestExtenderHello(&ExtenderHelloResult{})
		hello.Blackhole()
		loop := newTestHelloLoop(t, clock, directory, hello.Hello, []string{txt}, nil)
		settings := DefaultExtenderNetworkClientSettings()

		synctest.Wait()
		if !loop.networkClient.Status().InitialAttemptDone {
			t.Fatal("the first pass waited for hello before sampling")
		}
		if count, inFlight := hello.counts(); count != 1 || inFlight != 1 {
			t.Fatalf("hello reads = %d, %d out; expected the first read, still out", count, inFlight)
		}
		// the keys in force judged the bootstrap's record meanwhile
		if !testDirectoryAddressVerified(directory, "192.0.2.80") {
			t.Fatal("the bootstrap's record did not land verified under the keys in force")
		}

		for i := 0; i < 3; i += 1 {
			if elapsed := loop.pass(t); elapsed != 0 {
				t.Fatalf("a pass with hello out took %s", elapsed)
			}
		}
		if count, _ := hello.counts(); count != 1 {
			t.Fatalf("hello reads = %d, expected no second read while the first is out", count)
		}

		// the read runs out its budget beside the parked refresh loop
		time.Sleep(settings.HelloTimeout + time.Second)
		synctest.Wait()
		if count, inFlight := hello.counts(); count != 1 || inFlight != 0 {
			t.Fatalf("hello reads = %d, %d out; expected the first read to have failed", count, inFlight)
		}

		// after the failure: a pass in the backoff reads nothing, and the
		// pass that starts the next read does not wait for it either
		if elapsed := loop.pass(t); elapsed != 0 {
			t.Fatalf("a pass after the failure took %s", elapsed)
		}
		if count, _ := hello.counts(); count != 1 {
			t.Fatalf("hello reads = %d, expected none in the failure's backoff", count)
		}
		loop.clock.advance(settings.HelloMinBackoff)
		if elapsed := loop.pass(t); elapsed != 0 {
			t.Fatalf("the pass that started the next read took %s", elapsed)
		}
		if count, inFlight := hello.counts(); count != 2 || inFlight != 1 {
			t.Fatalf("hello reads = %d, %d out; expected the second read, out", count, inFlight)
		}

		// closing ends the read that is out and joins the hello loop
		loop.networkClient.Close()
		select {
		case <-loop.networkClient.helloDone:
		default:
			t.Fatal("Close returned before joining the hello loop")
		}
		if _, inFlight := hello.counts(); inFlight != 0 {
			t.Fatal("Close left a hello read out")
		}
	})
}

// After a failure no pass reads hello again until the backoff has passed, and
// each further failure on the path doubles the backoff, up to the max. When
// the operator answers again, the first read after the backoff installs its
// keys, which judge the bootstrap's records again at once, and an answer is
// read again only on the refresh period.
func TestExtenderNetworkClientReadsAFailedHelloAgainAfterItsBackoff(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		clock := newTestClock()
		directory, _ := newTestExtenderDirectory(t, clock, nil)
		// the keys the operator answers with once the path recovers, which
		// the directory does not hold yet
		rootPrivateKey, rootPublicKey := newTestRootKeyPair(t)
		txt := testExtenderDnsRecordTxt(t, rootPrivateKey, clock, "192.0.2.81")
		hello := newTestExtenderHello(&ExtenderHelloResult{})
		hello.Fail(errors.New("the operator is not reachable on this path"))
		loop := newTestHelloLoop(t, clock, directory, hello.Hello, []string{txt}, nil)
		settings := DefaultExtenderNetworkClientSettings()

		synctest.Wait()
		if count, _ := hello.counts(); count != 1 {
			t.Fatalf("hello reads = %d, expected the first", count)
		}
		for i := 0; i < 5; i += 1 {
			loop.pass(t)
		}
		if count, _ := hello.counts(); count != 1 {
			t.Fatalf("hello reads = %d after 5 passes, expected none in the backoff", count)
		}

		// each failure doubles the wait before the next read
		count := 1
		backoff := settings.HelloMinBackoff
		for backoff < settings.HelloMaxBackoff {
			loop.clock.advance(backoff - time.Second)
			loop.pass(t)
			if readCount, _ := hello.counts(); readCount != count {
				t.Fatalf("hello reads = %d before the %s backoff passed, expected %d", readCount, backoff, count)
			}
			loop.clock.advance(time.Second)
			loop.pass(t)
			count += 1
			if readCount, _ := hello.counts(); readCount != count {
				t.Fatalf("hello reads = %d once the %s backoff passed, expected %d", readCount, backoff, count)
			}
			backoff *= 2
		}
		// and never waits longer than the max
		loop.clock.advance(settings.HelloMaxBackoff - time.Second)
		loop.pass(t)
		if readCount, _ := hello.counts(); readCount != count {
			t.Fatalf("hello reads = %d before the max backoff passed, expected %d", readCount, count)
		}
		loop.clock.advance(time.Second)
		loop.pass(t)
		count += 1
		if readCount, _ := hello.counts(); readCount != count {
			t.Fatalf("hello reads = %d after the max backoff, expected %d", readCount, count)
		}
		if testDirectoryAddressVerified(directory, "192.0.2.81") {
			t.Fatal("a record the keys in force do not vouch for landed")
		}

		// the path recovers: the first read after the backoff answers, and
		// its keys judge the bootstrap's records at once
		hello.Answer(testHelloResultWithRootKeys(rootPublicKey))
		loop.clock.advance(settings.HelloMaxBackoff)
		passCount := loop.passCount.Load()
		loop.passes <- time.Time{}
		synctest.Wait()
		count += 1
		if readCount, _ := hello.counts(); readCount != count {
			t.Fatalf("hello reads = %d once the backoff passed, expected %d", readCount, count)
		}
		if !testDirectoryAddressVerified(directory, "192.0.2.81") {
			t.Fatal("the record of the operator's keys did not land once hello answered")
		}
		// the read went out with the pass that woke it, and the keys it
		// installed woke one more
		if passes := loop.passCount.Load() - passCount; passes != 2 {
			t.Fatalf("passes = %d, expected the one let and the one the keys woke", passes)
		}

		// an answer holds for the refresh period, and the same keys again
		// are not installed again: the bootstrap due on the same period runs
		// once, and no pass more is woken (pass)
		loop.clock.advance(settings.RebootstrapTimeout - time.Second)
		loop.pass(t)
		if readCount, _ := hello.counts(); readCount != count {
			t.Fatalf("hello reads = %d within the refresh period, expected %d", readCount, count)
		}
		txtCount := loop.txtCount.Load()
		loop.clock.advance(time.Second)
		loop.pass(t)
		if readCount, _ := hello.counts(); readCount != count+1 {
			t.Fatalf("hello reads = %d after the refresh period, expected %d", readCount, count+1)
		}
		if txts := loop.txtCount.Load() - txtCount; txts != 1 {
			t.Fatalf("txt resolutions = %d on the refresh period, expected 1", txts)
		}
	})
}

// A path change reads a failed hello again at once, whatever backoff the
// failures on the old path had built up, and a failure on the new path backs
// off from the minimum. A path where the operator answers gets the keys. An
// answer is not read again for a path change: neither the root keys nor the
// gossip identity depend on the path.
func TestExtenderNetworkClientReadsAFailedHelloAgainAfterAPathChange(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		clock := newTestClock()
		directory, _ := newTestExtenderDirectory(t, clock, nil)
		rootPrivateKey, rootPublicKey := newTestRootKeyPair(t)
		txt := testExtenderDnsRecordTxt(t, rootPrivateKey, clock, "192.0.2.82")
		hello := newTestExtenderHello(&ExtenderHelloResult{})
		hello.Fail(errors.New("the operator is not reachable on this path"))
		loop := newTestHelloLoop(t, clock, directory, hello.Hello, []string{txt}, nil)
		settings := DefaultExtenderNetworkClientSettings()

		synctest.Wait()
		// three failures on this path: the next waits four times the minimum
		loop.clock.advance(settings.HelloMinBackoff)
		loop.pass(t)
		loop.clock.advance(2 * settings.HelloMinBackoff)
		loop.pass(t)
		if count, _ := hello.counts(); count != 3 {
			t.Fatalf("hello reads = %d, expected 3", count)
		}
		loop.pass(t)
		if count, _ := hello.counts(); count != 3 {
			t.Fatalf("hello reads = %d, expected none in the backoff", count)
		}

		// the clock has not moved: only the path change reads it
		loop.networkClient.networkChanged()
		synctest.Wait()
		if count, _ := hello.counts(); count != 4 {
			t.Fatalf("hello reads = %d after the path change, expected 4", count)
		}
		// the failure on the new path waits the minimum, not eight times it
		loop.pass(t)
		if count, _ := hello.counts(); count != 4 {
			t.Fatalf("hello reads = %d, expected none in the new path's backoff", count)
		}
		loop.clock.advance(settings.HelloMinBackoff)
		loop.pass(t)
		if count, _ := hello.counts(); count != 5 {
			t.Fatalf("hello reads = %d once the minimum backoff passed, expected 5", count)
		}

		// a path where the operator answers gets the keys at once
		hello.Answer(testHelloResultWithRootKeys(rootPublicKey))
		loop.networkClient.networkChanged()
		synctest.Wait()
		if count, _ := hello.counts(); count != 6 {
			t.Fatalf("hello reads = %d after the second path change, expected 6", count)
		}
		if !testDirectoryAddressVerified(directory, "192.0.2.82") {
			t.Fatal("the record of the operator's keys did not land on the path where hello answered")
		}

		// an answer holds across a path change
		loop.networkClient.networkChanged()
		synctest.Wait()
		if count, _ := hello.counts(); count != 6 {
			t.Fatalf("hello reads = %d after a path change with an answer in force, expected 6", count)
		}
	})
}

// A path change wakes the hello loop itself, so the read does not wait for a
// pass the refresh loop is still busy with: here a bootstrap the resolver
// holds.
func TestExtenderNetworkClientPathChangeReadsTheHelloWhileAPassIsBusy(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		clock := newTestClock()
		directory, _ := newTestExtenderDirectory(t, clock, nil)
		hello := newTestExtenderHello(&ExtenderHelloResult{})
		hello.Fail(errors.New("the operator is not reachable on this path"))
		var holdBootstrap atomic.Bool
		bootstrapHeld := make(chan struct{})
		releaseBootstrap := make(chan struct{})
		loop := newTestHelloLoop(t, clock, directory, hello.Hello, nil, func(settings *ExtenderNetworkClientSettings) {
			// below the low-water mark, so the held pass bootstraps
			settings.LowWaterCount = 4
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
		if count, _ := hello.counts(); count != 1 {
			t.Fatalf("hello reads = %d, expected none in the backoff", count)
		}

		loop.networkClient.networkChanged()
		synctest.Wait()
		if count, _ := hello.counts(); count != 2 {
			t.Fatalf("hello reads = %d after the path change, expected the read while the pass is held", count)
		}

		holdBootstrap.Store(false)
		close(releaseBootstrap)
		synctest.Wait()
	})
}

// Where no root keys are in force nothing a pass applies can verify, so the
// first pass waits for the first hello read, as it did when hello was read
// ahead of it, and takes its bootstrap and its sample under the keys hello
// brings: the TXT records land judged under them (another root's refused),
// the first probe pass measures the whole set those keys vouch for, hinted
// continent first, and the first attempt is one that had the records. The
// keys landed before the pass looked at them, so they wake no second pass.
//
// The flake this replaces: the first pass went on without keys, released the
// probe gate and the first attempt on a bootstrap that had judged nothing, and
// the records landed in a pass the install woke, under probe passes that
// measured whichever had landed and while the failure counts the test read
// were still being written.
func TestExtenderNetworkClientFirstPassWaitsForTheFirstHelloWithNoKeysInForce(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		clock := newTestClock()
		directory := newTestUnanchoredExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
			// a failed feed dial does not hold the records the probe measures
			settings.HoldTimeout = 0
			settings.MaxHoldTimeout = 0
		})
		rootPrivateKey, rootPublicKey := newTestRootKeyPair(t)
		otherRootPrivateKey, _ := newTestRootKeyPair(t)
		txts := []string{
			testExtenderDnsRecordTxtWithContinent(t, rootPrivateKey, clock, "NA", "192.0.2.20"),
			testExtenderDnsRecordTxtWithContinent(t, rootPrivateKey, clock, "NA", "192.0.2.21"),
			testExtenderDnsRecordTxtWithContinent(t, rootPrivateKey, clock, "EU", "192.0.2.10"),
			testExtenderDnsRecordTxtWithContinent(t, rootPrivateKey, clock, "EU", "192.0.2.11"),
			testExtenderDnsRecordTxt(t, otherRootPrivateKey, clock, "192.0.2.84"),
		}
		probes := newTestProbeLog(map[string]time.Duration{
			"192.0.2.10": 20 * time.Millisecond,
			"192.0.2.11": 25 * time.Millisecond,
			"192.0.2.20": 120 * time.Millisecond,
			"192.0.2.21": 130 * time.Millisecond,
		})
		hello := newTestExtenderHello(testHelloResultWithRootKeys(rootPublicKey))
		release := hello.Hold()
		loop := newTestHelloLoop(t, clock, directory, hello.Hello, txts, func(settings *ExtenderNetworkClientSettings) {
			settings.IpVersionSupported = func(ipVersion int) bool { return ipVersion == 4 }
			settings.ProbeWindowCount = 2
			settings.ProbeMaxCandidateCount = 8
			settings.ProbeCountPerExtender = 1
			settings.ProbeCloseFactor = 2
			settings.ProbeCloseFloor = 10 * time.Millisecond
			settings.Probe = probes.probe
			settings.Hint = func(context.Context) (*ExtenderHintResult, error) {
				return &ExtenderHintResult{ContinentCode: "eu"}, nil
			}
		})

		synctest.Wait()
		if loop.networkClient.Status().InitialAttemptDone {
			t.Fatal("the first pass went on without keys while the first hello read was out")
		}
		if count, inFlight := hello.counts(); count != 1 || inFlight != 1 {
			t.Fatalf("hello reads = %d, %d out; expected the first read, still out", count, inFlight)
		}
		if count := loop.txtCount.Load(); count != 0 {
			t.Fatalf("txt resolutions = %d before hello answered, expected none", count)
		}
		if count := probes.count(); count != 0 {
			t.Fatalf("%d probes ran before any record verified", count)
		}

		release()
		synctest.Wait()
		if !loop.networkClient.Status().InitialAttemptDone {
			t.Fatal("the first pass did not go on once hello answered")
		}
		if count := loop.txtCount.Load(); count != 1 {
			t.Fatalf("txt resolutions = %d, expected the first bootstrap's alone, under hello's keys", count)
		}
		if passCount := loop.passCount.Load(); passCount != 1 {
			t.Fatalf("passes = %d, expected the first alone: its keys were in force before it looked", passCount)
		}
		for _, ip := range []string{"192.0.2.10", "192.0.2.11", "192.0.2.20", "192.0.2.21"} {
			if !testDirectoryAddressVerified(directory, ip) {
				t.Fatalf("the record of %s, which hello's keys sign, did not land verified", ip)
			}
		}
		if testDirectoryAddressVerified(directory, "192.0.2.84") {
			t.Fatal("the record another root signed landed")
		}
		ips := func() []string {
			probes.stateLock.Lock()
			defer probes.stateLock.Unlock()
			return slices.Sorted(slices.Values(probes.ips))
		}()
		if !slices.Equal(ips, []string{"192.0.2.10", "192.0.2.11"}) {
			t.Fatalf("probes = %v, expected exactly the two extenders of the hinted continent", ips)
		}
	})
}

// With no root keys in force the first pass waits for the first hello read
// only as long as the read lasts: a black-holed read costs the first pass its
// budget, and no pass after it. With no keys the TXT records wait, not
// resolved, rather than being refused and lost, and the keys a later read
// brings wake the refresh loop, whose bootstrap judges the records under them:
// what they sign lands verified, what another root signed is refused.
func TestExtenderNetworkClientTxtRecordsWaitForTheHelloKeys(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		clock := newTestClock()
		directory := newTestUnanchoredExtenderDirectory(t, clock, nil)
		rootPrivateKey, rootPublicKey := newTestRootKeyPair(t)
		otherRootPrivateKey, _ := newTestRootKeyPair(t)
		txts := []string{
			testExtenderDnsRecordTxt(t, rootPrivateKey, clock, "192.0.2.83"),
			testExtenderDnsRecordTxt(t, otherRootPrivateKey, clock, "192.0.2.84"),
		}
		hello := newTestExtenderHello(&ExtenderHelloResult{})
		hello.Blackhole()
		loop := newTestHelloLoop(t, clock, directory, hello.Hello, txts, nil)
		settings := DefaultExtenderNetworkClientSettings()

		time.Sleep(settings.HelloTimeout - time.Second)
		synctest.Wait()
		if loop.networkClient.Status().InitialAttemptDone {
			t.Fatal("the first pass went on without keys while the first hello read was out")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if !loop.networkClient.Status().InitialAttemptDone {
			t.Fatal("the first pass waited past the first hello read's budget")
		}
		if count := loop.txtCount.Load(); count != 0 {
			t.Fatalf("txt resolutions = %d with no root keys in force, expected the records to wait", count)
		}
		if elapsed := loop.pass(t); elapsed != 0 {
			t.Fatalf("a pass after the failed read took %s", elapsed)
		}
		if count := loop.txtCount.Load(); count != 0 {
			t.Fatalf("txt resolutions = %d with no root keys in force, expected the records to wait", count)
		}

		// the operator answers once the failure's backoff has passed
		hello.Answer(testHelloResultWithRootKeys(rootPublicKey))
		loop.clock.advance(settings.HelloMinBackoff)
		passCount := loop.passCount.Load()
		loop.passes <- time.Time{}
		synctest.Wait()
		// the read went out with the pass that woke it, and the keys it
		// installed woke one more
		if passes := loop.passCount.Load() - passCount; passes != 2 {
			t.Fatalf("passes = %d, expected the one let and the one the keys woke", passes)
		}
		if count := loop.txtCount.Load(); count != 1 {
			t.Fatalf("txt resolutions = %d once hello's keys were in force, expected 1", count)
		}
		if !testDirectoryAddressVerified(directory, "192.0.2.83") {
			t.Fatal("the record hello's keys sign did not land verified")
		}
		if testDirectoryAddressVerified(directory, "192.0.2.84") {
			t.Fatal("the record another root signed landed")
		}
	})
}

// A path change wakes the hello loop through the wake it shares with the hint
// loop, so every client that can take a path change wakes hello too: one
// built bare included, as the probe tests build one to drive a pass by hand.
// Before, the hello loop had a wake of its own, and a path change panicked on
// a client built without it.
func TestExtenderNetworkClientPathChangeWakesTheHelloLoopOfABareClient(t *testing.T) {
	clock := newTestClock()
	directory, _ := newTestExtenderDirectory(t, clock, nil)
	networkClient := newTestBareExtenderNetworkClient(t, directory)
	hintWake := networkClient.hintWake.NotifyChannel()
	networkClient.networkChanged()
	select {
	case <-hintWake:
	default:
		t.Fatal("the path change did not wake the hint and hello loops")
	}
	helloRearmed := func() bool {
		networkClient.stateLock.Lock()
		defer networkClient.stateLock.Unlock()
		return networkClient.helloRearmed
	}()
	if !helloRearmed {
		t.Fatal("the path change did not ask for a failed hello again")
	}
}

// A pass verifies under the keys in force while hello is out -- here a cached
// anchor -- and keys that hello installs judge everything again: a record only
// the retired key vouched for stops verifying, and the bootstrap judges its
// TXT records again under the new keys at once, so the record they sign,
// refused under the old ones, lands.
func TestExtenderNetworkClientHelloKeysJudgeTheBootstrapAgain(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		clock := newTestClock()
		// the keys in force before hello, as a bundled or configured anchor
		directory, cachedRootPrivateKey := newTestExtenderDirectory(t, clock, nil)
		rootPrivateKey, rootPublicKey := newTestRootKeyPair(t)
		txts := []string{
			testExtenderDnsRecordTxt(t, cachedRootPrivateKey, clock, "192.0.2.85"),
			testExtenderDnsRecordTxt(t, rootPrivateKey, clock, "192.0.2.86"),
		}
		hello := newTestExtenderHello(testHelloResultWithRootKeys(rootPublicKey))
		release := hello.Hold()
		loop := newTestHelloLoop(t, clock, directory, hello.Hello, txts, nil)

		synctest.Wait()
		if !loop.networkClient.Status().InitialAttemptDone {
			t.Fatal("the first pass waited for hello")
		}
		if count := loop.txtCount.Load(); count != 1 {
			t.Fatalf("txt resolutions = %d, expected the first pass to judge them under the keys in force", count)
		}
		if !testDirectoryAddressVerified(directory, "192.0.2.85") {
			t.Fatal("the record of the keys in force did not land verified")
		}
		if testDirectoryAddressVerified(directory, "192.0.2.86") {
			t.Fatal("a record no key in force signs landed")
		}

		// hello answers with keys that retire the cached one
		release()
		synctest.Wait()
		if testDirectoryAddressVerified(directory, "192.0.2.85") {
			t.Fatal("the record only the retired key signs still verifies")
		}
		if directory.AddressUsable(netip.MustParseAddr("192.0.2.85")) {
			t.Fatal("the address only the retired key vouched for is still usable")
		}
		if count := loop.txtCount.Load(); count != 2 {
			t.Fatalf("txt resolutions = %d, expected the new keys to judge them again", count)
		}
		if !testDirectoryAddressVerified(directory, "192.0.2.86") {
			t.Fatal("the record hello's keys sign did not land once they were in force")
		}
		if !directory.RootKeys().Equal(NewExtenderRootKeySet(rootPublicKey)) {
			t.Fatal("the keys in force are not hello's")
		}
	})
}

// A pass chooses its candidates under the root keys in force, and keys that
// hello installs while the pass dials may no longer vouch for the rest of
// them: the pass dials no further candidate, and the next pass, which the
// install woke, chooses under the new keys. Here hello answers with a rotation
// that retires the key alone vouching for the second candidate while the
// first candidate's dial is out.
func TestExtenderNetworkClientDialsNoCandidateChosenUnderReplacedRootKeys(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		clock := newTestClock()
		directory, keptRootPrivateKey := newTestExtenderDirectory(t, clock, nil)
		keptRootPublicKey := keptRootPrivateKey.Public().(ed25519.PublicKey)
		retiredRootPrivateKey, retiredRootPublicKey := newTestRootKeyPair(t)
		directory.SetRootKeys(NewExtenderRootKeySet(keptRootPublicKey, retiredRootPublicKey))
		candidates := []struct {
			rootPrivateKey ed25519.PrivateKey
			ip             string
		}{
			// first in the candidate order, by its address
			{rootPrivateKey: keptRootPrivateKey, ip: "192.0.2.96"},
			{rootPrivateKey: retiredRootPrivateKey, ip: "192.0.2.97"},
		}
		for _, c := range candidates {
			record := signTestRecord(
				t,
				c.rootPrivateKey,
				newTestExtenderKey(t),
				clock.Now(),
				clock.Now().Add(14*24*time.Hour),
				testExtenderAddress(c.ip, ExtenderCarrierTcp),
			)
			if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
				t.Fatal(err)
			}
		}

		// every carrier dial fails; the first is held until the test lets it
		var stateLock sync.Mutex
		dialAddrs := []string{}
		releaseDial := make(chan struct{})
		dials := func() []string {
			stateLock.Lock()
			defer stateLock.Unlock()
			return slices.Clone(dialAddrs)
		}
		strategySettings := DefaultClientStrategySettings()
		strategySettings.ConnectSettings.DialContextSettings = &DialContextSettings{
			DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
				first := func() bool {
					stateLock.Lock()
					defer stateLock.Unlock()
					dialAddrs = append(dialAddrs, addr)
					return len(dialAddrs) == 1
				}()
				if first {
					select {
					case <-releaseDial:
					case <-ctx.Done():
					}
				}
				return nil, errors.New("no route in this test")
			},
			PacketConnFactory: func(ctx context.Context) (net.PacketConn, error) {
				return nil, errors.New("no packet endpoint in this test")
			},
		}
		clientStrategy := NewClientStrategy(t.Context(), strategySettings)
		t.Cleanup(clientStrategy.Close)

		hello := newTestExtenderHello(&ExtenderHelloResult{
			RootPublicKeyHexes: []string{ExtenderKeySeedHex(keptRootPublicKey)},
			GossipPeerId:       testExtenderGossipPeerId,
		})
		releaseHello := hello.Hold()
		var passCount atomic.Int64
		settings := DefaultExtenderNetworkClientSettings()
		settings.Log = NewNoopLogger()
		settings.Now = clock.Now
		settings.ExtenderDnsName = "hello-loop.example"
		settings.ProbeWindowCount = 0
		settings.IpVersionSupported = func(ipVersion int) bool { return ipVersion == 4 }
		settings.PassAfter = func(time.Duration) <-chan time.Time {
			passCount.Add(1)
			// only a wake starts another pass
			return make(chan time.Time)
		}
		settings.Hello = hello.Hello
		settings.Hint = func(context.Context) (*ExtenderHintResult, error) { return &ExtenderHintResult{}, nil }
		settings.ResolveDns = func(context.Context, string) ([]netip.Addr, error) { return nil, nil }
		settings.ResolveDnsTxt = func(context.Context, string) ([]string, error) { return nil, nil }
		networkClient := NewExtenderNetworkClient(t.Context(), clientStrategy, directory, settings)
		t.Cleanup(networkClient.Close)

		synctest.Wait()
		if addrs := dials(); !slices.Equal(addrs, []string{"192.0.2.96:443"}) {
			t.Fatalf("dials = %v, expected the first candidate's, held", addrs)
		}
		releaseHello()
		synctest.Wait()
		if networkClient.Status().GossipPeerId != testExtenderGossipPeerId {
			t.Fatal("hello did not answer while the dial was out")
		}
		if testDirectoryAddressVerified(directory, "192.0.2.97") {
			t.Fatal("the retired key still vouches for the second candidate")
		}

		close(releaseDial)
		synctest.Wait()
		if passCount.Load() == 0 {
			t.Fatal("the pass with the held dial did not end")
		}
		if addrs := dials(); slices.Contains(addrs, "192.0.2.97:443") {
			t.Fatalf("dials = %v; the second candidate was dialed after the keys that chose it were replaced", addrs)
		}
	})
}

// A hello that answers with blank root keys alone leaves the anchor in force,
// as one that answers with none does. Installed, the empty set they parse to
// would refuse every record until the next answer, a refresh period later.
func TestExtenderNetworkClientKeepsTheAnchorWhenHelloAnswersBlankRootKeys(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		clock := newTestClock()
		directory, rootPrivateKey := newTestExtenderDirectory(t, clock, nil)
		rootKeySet := directory.RootKeys()
		hello := newTestExtenderHello(&ExtenderHelloResult{
			RootPublicKeyHexes: []string{"", "  "},
			GossipPeerId:       testExtenderGossipPeerId,
		})
		loop := newTestHelloLoop(t, clock, directory, hello.Hello, nil, nil)

		synctest.Wait()
		if loop.networkClient.Status().GossipPeerId != testExtenderGossipPeerId {
			t.Fatal("hello did not answer")
		}
		if directory.RootKeys() != rootKeySet {
			t.Fatalf("root keys = %d, expected the anchor still in force", directory.RootKeys().Len())
		}
		record := signTestRecord(
			t,
			rootPrivateKey,
			newTestExtenderKey(t),
			clock.Now(),
			clock.Now().Add(14*24*time.Hour),
			testExtenderAddress("192.0.2.98"),
		)
		if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
			t.Fatalf("a record the anchor signs was refused after hello answered: %v", err)
		}
	})
}

// A record verified under root keys that SetRootKeys replaced before it was
// stored is judged again under the keys in force. This is the interleaving
// ApplyRecord meets when hello installs keys while a pass applies a record,
// taken in order: the verification, the install, then the store. A record
// only the replaced keys sign is refused, and one the new keys also sign
// lands.
func TestExtenderDirectoryJudgesARecordAgainUnderKeysReplacedBeforeItsStore(t *testing.T) {
	clock := newTestClock()
	directory, retiredRootPrivateKey := newTestExtenderDirectory(t, clock, nil)
	retiredRootPublicKey := retiredRootPrivateKey.Public().(ed25519.PublicKey)
	_, rootPublicKey := newTestRootKeyPair(t)
	signRecord := func(ip string) *protocol.ExtenderRecord {
		return signTestRecord(
			t,
			retiredRootPrivateKey,
			newTestExtenderKey(t),
			clock.Now(),
			clock.Now().Add(14*24*time.Hour),
			testExtenderAddress(ip),
		)
	}

	// replaced by keys without the one that signed the record
	verifiedRootKeySet := directory.RootKeys()
	record := signRecord("192.0.2.92")
	body, err := verifiedRootKeySet.VerifyRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	directory.SetRootKeys(NewExtenderRootKeySet(rootPublicKey))
	if changed, err := directory.applyVerifiedRecord(record, body, verifiedRootKeySet, 0, ExtenderSourceFeed); err == nil || changed {
		t.Fatalf("changed = %t, err = %v; expected the record refused under the keys in force", changed, err)
	}
	if testDirectoryAddressVerified(directory, "192.0.2.92") {
		t.Fatal("a record only the replaced keys sign landed after them")
	}

	// replaced by keys that keep the one that signed the record
	verifiedRootKeySet = NewExtenderRootKeySet(retiredRootPublicKey, rootPublicKey)
	directory.SetRootKeys(verifiedRootKeySet)
	record = signRecord("192.0.2.93")
	body, err = verifiedRootKeySet.VerifyRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	directory.SetRootKeys(NewExtenderRootKeySet(rootPublicKey, retiredRootPublicKey))
	if changed, err := directory.applyVerifiedRecord(record, body, verifiedRootKeySet, 0, ExtenderSourceFeed); err != nil || !changed {
		t.Fatalf("changed = %t, err = %v; expected the record to land under keys that keep its signer", changed, err)
	}
	if !testDirectoryAddressVerified(directory, "192.0.2.93") {
		t.Fatal("the record the keys in force sign did not land")
	}
}

// A revocation verified under root keys that SetRootKeys replaced before it
// was stored is judged again under the keys in force, as a record is: one only
// the replaced keys sign is refused, and the key it names stays active.
func TestExtenderDirectoryJudgesARevocationAgainUnderKeysReplacedBeforeItsStore(t *testing.T) {
	clock := newTestClock()
	directory, retiredRootPrivateKey := newTestExtenderDirectory(t, clock, nil)
	retiredRootPublicKey := retiredRootPrivateKey.Public().(ed25519.PublicKey)
	rootPrivateKey, rootPublicKey := newTestRootKeyPair(t)
	retiringRootKeySet := NewExtenderRootKeySet(retiredRootPublicKey, rootPublicKey)
	directory.SetRootKeys(retiringRootKeySet)

	extenderPublicKey := newTestExtenderKey(t)
	record := signTestRecord(
		t,
		rootPrivateKey,
		extenderPublicKey,
		clock.Now(),
		clock.Now().Add(14*24*time.Hour),
		testExtenderAddress("192.0.2.94"),
	)
	if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	revocation := signTestRevocation(t, retiredRootPrivateKey, extenderPublicKey, clock.Now().Add(time.Second))
	body, err := retiringRootKeySet.VerifyRevocation(revocation)
	if err != nil {
		t.Fatal(err)
	}
	// the rotation completes, retiring the key that signed the revocation
	directory.SetRootKeys(NewExtenderRootKeySet(rootPublicKey))
	if changed, err := directory.applyVerifiedRevocation(revocation, body, retiringRootKeySet, 0, ExtenderSourceFeed); err == nil || changed {
		t.Fatalf("changed = %t, err = %v; expected the revocation refused under the keys in force", changed, err)
	}
	if state := testDirectoryState(t, directory, netip.MustParseAddr("192.0.2.94")); state != ExtenderStateActive {
		t.Fatalf("state = %s, expected the record to stay active", state)
	}
}

// The schedule alone, as hello uses it: a path change makes a failed read due
// at once, backing off from the minimum again, and leaves an answer's refresh
// period alone.
func TestExtenderReadScheduleClearsAFailedReadsBackoff(t *testing.T) {
	refreshTimeout := 6 * time.Hour
	minBackoff := time.Minute
	maxBackoff := 5 * time.Minute
	readSchedule := newExtenderReadSchedule(refreshTimeout, minBackoff, maxBackoff)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	assertNextDue := func(wait time.Duration) {
		t.Helper()
		if readSchedule.Due(now.Add(wait - time.Nanosecond)) {
			t.Fatalf("due before %s", wait)
		}
		if !readSchedule.Due(now.Add(wait)) {
			t.Fatalf("not due after %s", wait)
		}
	}

	readSchedule.Fail(now)
	readSchedule.Fail(now)
	assertNextDue(2 * minBackoff)
	readSchedule.ClearBackoff()
	if !readSchedule.Due(now) {
		t.Fatal("a failed read is not due after the backoff was cleared")
	}
	readSchedule.Fail(now)
	assertNextDue(minBackoff)

	readSchedule.Answer(now)
	readSchedule.ClearBackoff()
	assertNextDue(refreshTimeout)

	// before any read, it is due
	readSchedule = newExtenderReadSchedule(refreshTimeout, minBackoff, maxBackoff)
	readSchedule.ClearBackoff()
	if !readSchedule.Due(now) {
		t.Fatal("not due at a start")
	}
}

// Two key sets are equal when they accept the same keys, in any order.
func TestExtenderRootKeySetEqual(t *testing.T) {
	_, firstRootPublicKey := newTestRootKeyPair(t)
	_, secondRootPublicKey := newTestRootKeyPair(t)
	_, thirdRootPublicKey := newTestRootKeyPair(t)
	cases := []struct {
		description string
		keySet      *ExtenderRootKeySet
		otherKeySet *ExtenderRootKeySet
		equal       bool
	}{
		{
			description: "the same keys in another order",
			keySet:      NewExtenderRootKeySet(firstRootPublicKey, secondRootPublicKey),
			otherKeySet: NewExtenderRootKeySet(secondRootPublicKey, firstRootPublicKey),
			equal:       true,
		},
		{
			description: "two empty sets",
			keySet:      NewExtenderRootKeySet(),
			otherKeySet: NewExtenderRootKeySet(),
			equal:       true,
		},
		{
			description: "sets with a different key",
			keySet:      NewExtenderRootKeySet(firstRootPublicKey, secondRootPublicKey),
			otherKeySet: NewExtenderRootKeySet(firstRootPublicKey, thirdRootPublicKey),
			equal:       false,
		},
		{
			description: "a set and its superset",
			keySet:      NewExtenderRootKeySet(firstRootPublicKey),
			otherKeySet: NewExtenderRootKeySet(firstRootPublicKey, secondRootPublicKey),
			equal:       false,
		},
		{
			description: "a set and its subset",
			keySet:      NewExtenderRootKeySet(firstRootPublicKey, secondRootPublicKey),
			otherKeySet: NewExtenderRootKeySet(firstRootPublicKey),
			equal:       false,
		},
		{
			description: "an empty set and a set with a key",
			keySet:      NewExtenderRootKeySet(),
			otherKeySet: NewExtenderRootKeySet(firstRootPublicKey),
			equal:       false,
		},
	}
	for _, c := range cases {
		if equal := c.keySet.Equal(c.otherKeySet); equal != c.equal {
			t.Errorf("%s: equal = %t, expected %t", c.description, equal, c.equal)
		}
	}
}
