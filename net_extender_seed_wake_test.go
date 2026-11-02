package connect

import (
	"errors"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"
)

// Waking the network client after a seed apply (EXTENDER-UPDATE2.md §2.3): the
// control route was unreachable, so hello failed and backed off; once a seed
// has made extenders dialable, Wake reads hello again at once and runs a pass,
// without waiting out the backoff, and without the path change's other
// effects.
func TestExtenderNetworkClientWakeReadsAFailedHelloAgain(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		clock := newTestClock()
		directory, _ := newTestExtenderDirectory(t, clock, nil)
		rootPrivateKey, rootPublicKey := newTestRootKeyPair(t)
		txt := testExtenderDnsRecordTxt(t, rootPrivateKey, clock, "192.0.2.91")
		hello := newTestExtenderHello(&ExtenderHelloResult{})
		hello.Fail(errors.New("the control route is unreachable"))
		loop := newTestHelloLoop(t, clock, directory, hello.Hello, []string{txt}, nil)
		settings := DefaultExtenderNetworkClientSettings()

		synctest.Wait()
		// failures build up a backoff of twice the minimum
		loop.clock.advance(settings.HelloMinBackoff)
		loop.pass(t)
		if count, _ := hello.counts(); count != 2 {
			t.Fatalf("hello reads = %d, expected 2", count)
		}
		loop.pass(t)
		if count, _ := hello.counts(); count != 2 {
			t.Fatalf("hello reads = %d, expected none in the backoff", count)
		}

		// the seed made extenders dialable and the owner wakes the client: hello
		// is read again at once, and the refresh loop runs a pass on its own
		hello.Answer(testHelloResultWithRootKeys(rootPublicKey))
		passCount := loop.passCount.Load()
		loop.networkClient.Wake()
		synctest.Wait()
		if count, _ := hello.counts(); count != 3 {
			t.Fatalf("hello reads = %d after the wake, expected 3", count)
		}
		if passes := loop.passCount.Load() - passCount; passes < 1 {
			t.Fatal("the wake did not run a pass")
		}
		if !testDirectoryAddressVerified(directory, "192.0.2.91") {
			t.Fatal("the keys hello brought after the wake did not land")
		}

		// a latency sample, which a path change would make due a refresh, stays
		// current across a wake; and with hello answered, a wake does not read
		// it again
		ip := netip.MustParseAddr("192.0.2.91")
		directory.RecordLatency(ip, 20*time.Millisecond, false)
		loop.networkClient.Wake()
		synctest.Wait()
		if count, _ := hello.counts(); count != 3 {
			t.Fatalf("hello reads = %d after a wake with an answer in force, expected 3", count)
		}
		sampled := false
		for _, candidate := range directory.Candidates(0, 16) {
			if candidate.Ip != ip {
				continue
			}
			sampled = candidate.Latency == 20*time.Millisecond
			if candidate.LatencyRefreshDue {
				t.Fatal("the wake made a latency sample due a refresh, as only a path change should")
			}
		}
		if !sampled {
			t.Fatal("the latency sample is not on the candidate")
		}
	})
}
