// The DMCA detector's idle scan parks while it tracks no flow, and the first
// tracked flow wakes it. Every event runs in virtual time.
package connect

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// With no flow tracked the scan takes no wake; a tracked flow wakes it, is
// reclaimed within FlowTtl and one scan interval of its last packet, and the
// scan parks again.
func TestDmcaIdleScanParksWithoutFlows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		settings := DefaultDmcaSecurityPolicySettings()
		scans := atomic.Int64{}
		settings.afterFlowScanForTest = func() {
			scans.Add(1)
		}
		detector := newDmcaDetector(ctx, settings, newWebStandardDetector(DefaultWebStandardSettings()))
		defer func() {
			cancel()
			<-detector.runDone
		}()
		// a quarter of the default FlowTtl
		scanInterval := 75 * time.Second

		time.Sleep(10 * scanInterval)
		synctest.Wait()
		if got := scans.Load(); got != 0 {
			t.Fatalf("the idle scan ran %d times in %s with no flow tracked, want it parked", got, 10*scanInterval)
		}

		detector.classify(dmcaPath(IpProtocolUdp, 40001, 50000, false), []byte("x"))
		if detector.flowCount() != 1 {
			t.Fatal("the flow was not tracked")
		}
		time.Sleep(settings.FlowTtl + scanInterval)
		synctest.Wait()
		if count := detector.flowCount(); count != 0 {
			t.Fatalf("%d idle flows were not reclaimed within FlowTtl and one scan interval", count)
		}
		parkedAt := scans.Load()
		time.Sleep(10 * scanInterval)
		synctest.Wait()
		if got := scans.Load() - parkedAt; got != 0 {
			t.Fatalf("the scan ran %d more times after reclaiming its last flow, want it parked", got)
		}
	})
}
