// The shared read of the interface table behind the family probes
// (controlFamilyInterfaces). urnetwork/connect#227: a provider process that
// runs one provider per proxy read the host's interface table on every probe,
// once per family on every extender sampler pass of every provider, and on
// linux each read is an interface dump plus one address dump per interface. On
// a 1000-proxy host that was a fifth of the process's CPU. The clock is the
// test's and the concurrent case holds the first read on a barrier, so the
// read counts are exact and nothing waits on time.

package connect

import (
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A dual-stack host as the probe sees it. 198.18.0.0/15 (RFC 2544) and
// 3fff::/20 (RFC 9637) read as connectivity while naming no real network.
func testDualStackControlFamilyInterfaces() []controlFamilyInterface {
	return []controlFamilyInterface{
		{
			name:  "lo0",
			flags: net.FlagUp | net.FlagLoopback,
			addrs: []net.Addr{ipNet("127.0.0.1/8"), ipNet("::1/128")},
		},
		{
			name:  "en0",
			flags: net.FlagUp,
			addrs: []net.Addr{ipNet("198.18.7.20/24"), ipNet("3fff:db8::1/64")},
		},
	}
}

// The same host after it lost its v6 address.
func testIpv4OnlyControlFamilyInterfaces() []controlFamilyInterface {
	return []controlFamilyInterface{
		{
			name:  "lo0",
			flags: net.FlagUp | net.FlagLoopback,
			addrs: []net.Addr{ipNet("127.0.0.1/8"), ipNet("::1/128")},
		},
		{
			name:  "en0",
			flags: net.FlagUp,
			addrs: []net.Addr{ipNet("198.18.7.20/24")},
		},
	}
}

// A fake interface table that counts its reads. Safe for concurrent use.
type testControlFamilyInterfaceSource struct {
	readCount atomic.Int64

	stateLock  sync.Mutex
	interfaces []controlFamilyInterface
	err        error
}

func newTestControlFamilyInterfaceSource(
	interfaces []controlFamilyInterface,
	err error,
) *testControlFamilyInterfaceSource {
	return &testControlFamilyInterfaceSource{
		interfaces: interfaces,
		err:        err,
	}
}

func (self *testControlFamilyInterfaceSource) read() ([]controlFamilyInterface, error) {
	self.readCount.Add(1)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.interfaces, self.err
}

func (self *testControlFamilyInterfaceSource) set(interfaces []controlFamilyInterface, err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.interfaces = interfaces
	self.err = err
}

// Installs the settings and the source for one test, and returns the clock the
// shared read reads.
func testSwapControlFamilyRead(
	t *testing.T,
	interfaceReadReuseTimeout time.Duration,
	source *testControlFamilyInterfaceSource,
) *testClock {
	t.Helper()
	clock := newTestClock()
	t.Cleanup(swapControlFamilyProbeSettings(&controlFamilyProbeSettings{
		InterfaceReadReuseTimeout: interfaceReadReuseTimeout,
		Now:                       clock.Now,
	}))
	t.Cleanup(swapControlFamilyInterfaces(source.read))
	return clock
}

// The root cause of connect#227's interface dumps: every probe of every
// provider read the table. Within the reuse window the probes share one read.
func TestFamilyProbesShareOneInterfaceRead(t *testing.T) {
	source := newTestControlFamilyInterfaceSource(testDualStackControlFamilyInterfaces(), nil)
	testSwapControlFamilyRead(t, 2*time.Second, source)

	providerCount := 1000
	for range providerCount {
		if !probeFamilySupport(4) || !probeFamilySupport(6) {
			t.Fatal("a dual-stack host reported a missing family")
		}
	}
	if readCount := source.readCount.Load(); readCount != 1 {
		t.Fatalf(
			"%d probes read the interface table %d times, expected one read within the reuse window",
			2*providerCount,
			readCount,
		)
	}
}

// The path the profiles of connect#227 show: each provider's extender sampler
// asks for both families on every pass (feedCandidates). The samplers of a
// process share one read.
func TestExtenderSamplersInOneProcessShareOneInterfaceRead(t *testing.T) {
	source := newTestControlFamilyInterfaceSource(testDualStackControlFamilyInterfaces(), nil)
	clock := testSwapControlFamilyRead(t, 2*time.Second, source)

	directorySettings := DefaultExtenderDirectorySettings()
	directorySettings.Now = clock.Now
	directorySettings.NetworkHosts = []string{testExtenderNetworkHost}
	directory := NewExtenderDirectory(t.Context(), directorySettings)
	defer directory.Close()
	directory.AddManual(netip.MustParseAddr("198.18.7.30"))
	directory.AddManual(netip.MustParseAddr("3fff:db8::30"))

	providerCount := 1000
	for range providerCount {
		settings := DefaultExtenderNetworkClientSettings()
		settings.Now = clock.Now
		// the host's own probe, as a provider's sampler runs it
		settings.IpVersionSupported = nil
		networkClient := &ExtenderNetworkClient{
			ctx:           t.Context(),
			log:           NewNoopLogger(),
			directory:     directory,
			settings:      settings,
			statusMonitor: NewMonitorValue[ExtenderNetworkClientStatus](ExtenderNetworkClientStatus{}),
		}
		candidates, _ := networkClient.feedCandidates()
		if len(candidates) != 2 {
			t.Fatalf("a sampler found %d candidates, expected the v4 and the v6 address", len(candidates))
		}
	}
	if readCount := source.readCount.Load(); readCount != 1 {
		t.Fatalf(
			"%d sampler passes read the interface table %d times, expected one read within the reuse window",
			providerCount,
			readCount,
		)
	}
}

// Probes that start while a read is out wait for it rather than reading again,
// which is the startup of a process that brings up many providers at once.
func TestConcurrentFamilyProbesShareOneInterfaceRead(t *testing.T) {
	readEntered := make(chan struct{})
	readRelease := make(chan struct{})
	var readCount atomic.Int64
	clock := newTestClock()
	t.Cleanup(swapControlFamilyProbeSettings(&controlFamilyProbeSettings{
		InterfaceReadReuseTimeout: 2 * time.Second,
		Now:                       clock.Now,
	}))
	t.Cleanup(swapControlFamilyInterfaces(func() ([]controlFamilyInterface, error) {
		if readCount.Add(1) == 1 {
			close(readEntered)
			<-readRelease
		}
		return testDualStackControlFamilyInterfaces(), nil
	}))

	probeCount := 100
	results := make(chan bool, probeCount)
	go func() {
		results <- probeFamilySupport(6)
	}()
	<-readEntered
	// the first read is out and held: every probe from here starts while it
	// is out or after it, inside the window, and none of them reads
	for range probeCount - 1 {
		go func() {
			results <- probeFamilySupport(6)
		}()
	}
	close(readRelease)
	for range probeCount {
		if !<-results {
			t.Fatal("a probe that waited on the shared read reported no v6")
		}
	}
	if n := readCount.Load(); n != 1 {
		t.Fatalf("%d concurrent probes read the interface table %d times, expected one", probeCount, n)
	}
}

// A read that has not returned by the end of its window holds only the probes
// of that window: a later probe reads again rather than waiting on it.
func TestAProbeAfterTheWindowDoesNotWaitOnAReadStillOut(t *testing.T) {
	readEntered := make(chan struct{})
	readRelease := make(chan struct{})
	var readCount atomic.Int64
	clock := newTestClock()
	t.Cleanup(swapControlFamilyProbeSettings(&controlFamilyProbeSettings{
		InterfaceReadReuseTimeout: 2 * time.Second,
		Now:                       clock.Now,
	}))
	t.Cleanup(swapControlFamilyInterfaces(func() ([]controlFamilyInterface, error) {
		if readCount.Add(1) == 1 {
			close(readEntered)
			<-readRelease
		}
		return testDualStackControlFamilyInterfaces(), nil
	}))

	firstResult := make(chan bool, 1)
	go func() {
		firstResult <- probeFamilySupport(6)
	}()
	<-readEntered
	clock.advance(2 * time.Second)
	laterResult := make(chan bool, 1)
	go func() {
		laterResult <- probeFamilySupport(6)
	}()
	select {
	case found := <-laterResult:
		if !found {
			t.Fatal("the later probe reported no v6")
		}
	case <-time.After(30 * time.Second):
		close(readRelease)
		t.Fatal("a probe after the window waited on a read that was still out")
	}
	close(readRelease)
	if !<-firstResult {
		t.Fatal("the held read reported no v6")
	}
	if n := readCount.Load(); n != 2 {
		t.Fatalf("read the table %d times, expected the held read and one after its window", n)
	}
}

// A read answers for the reuse window and no longer: a host that lost an
// address without telling the process is seen at the end of the window.
func TestFamilyProbeReadsAgainAfterTheReuseWindow(t *testing.T) {
	source := newTestControlFamilyInterfaceSource(testDualStackControlFamilyInterfaces(), nil)
	clock := testSwapControlFamilyRead(t, 2*time.Second, source)

	if !probeFamilySupport(6) {
		t.Fatal("a dual-stack host reported no v6")
	}
	source.set(testIpv4OnlyControlFamilyInterfaces(), nil)
	clock.advance(2*time.Second - time.Nanosecond)
	if !probeFamilySupport(6) {
		t.Fatal("a probe inside the reuse window read the table again")
	}
	if n := source.readCount.Load(); n != 1 {
		t.Fatalf("read the table %d times inside the window, expected once", n)
	}
	clock.advance(time.Nanosecond)
	if probeFamilySupport(6) {
		t.Fatal("a probe after the reuse window answered from the old read")
	}
	if n := source.readCount.Load(); n != 2 {
		t.Fatalf("read the table %d times across the window, expected twice", n)
	}
}

// A network change drops the shared read at once: the old path's addresses are
// no evidence about the new path.
func TestNetworkChangedDropsTheSharedInterfaceRead(t *testing.T) {
	source := newTestControlFamilyInterfaceSource(testDualStackControlFamilyInterfaces(), nil)
	testSwapControlFamilyRead(t, 2*time.Second, source)
	controlFamilyClear()
	t.Cleanup(controlFamilyClear)

	if !probeFamilySupport(6) {
		t.Fatal("a dual-stack host reported no v6")
	}
	source.set(testIpv4OnlyControlFamilyInterfaces(), nil)
	NetworkChanged()
	if probeFamilySupport(6) {
		t.Fatal("a probe after a network change answered from the old path's table")
	}
	if n := source.readCount.Load(); n != 2 {
		t.Fatalf("read the table %d times, expected once before and once after the change", n)
	}
}

// A failed read is no evidence and is not kept: the probes that waited on it
// fail closed, and the next probe reads again rather than taking the failure
// for the rest of the window.
func TestAFailedInterfaceReadIsNotReused(t *testing.T) {
	source := newTestControlFamilyInterfaceSource(nil, errors.New("operation not permitted"))
	testSwapControlFamilyRead(t, 2*time.Second, source)

	if probeFamilySupport(4) {
		t.Fatal("a probe said yes with no readable interface table")
	}
	if _, readable := probeFamilyAddressReadable(4, nil); readable {
		t.Fatal("a failed read was reported readable")
	}
	source.set(testDualStackControlFamilyInterfaces(), nil)
	if !probeFamilySupport(4) {
		t.Fatal("a probe after a failed read kept the failure")
	}
	if n := source.readCount.Load(); n != 3 {
		t.Fatalf("read the table %d times, expected every failed read to be read again", n)
	}
}

// A non-positive reuse timeout reads the table for every probe.
func TestANonPositiveInterfaceReadReuseTimeoutReadsForEveryProbe(t *testing.T) {
	source := newTestControlFamilyInterfaceSource(testDualStackControlFamilyInterfaces(), nil)
	testSwapControlFamilyRead(t, 0, source)

	for range 3 {
		if !probeFamilySupport(4) {
			t.Fatal("a dual-stack host reported no v4")
		}
	}
	if n := source.readCount.Load(); n != 3 {
		t.Fatalf("read the table %d times for 3 probes, expected 3", n)
	}
}
