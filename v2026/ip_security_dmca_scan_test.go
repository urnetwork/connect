// The DMCA detector's idle scan parks while it tracks no flow, and the first
// tracked flow wakes it. Flows age on the settings clock. Every event runs in
// virtual time.
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

// A flow ages on the settings clock, not on the clock that times the scans.
// While the settings clock holds, every scan keeps the flow however long the
// scans run; a packet refreshes the flow at the settings clock's time; and the
// first scan after the settings clock passes FlowTtl from that packet reclaims
// it. A caller that fills the table slowly therefore cannot age it mid-fill.
func TestDmcaIdleScanAgesFlowsOnTheSettingsClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		settings := DefaultDmcaSecurityPolicySettings()
		settingsStart := time.Unix(1_000_000, 0)
		var settingsElapsed atomic.Int64
		settings.Now = func() time.Time {
			return settingsStart.Add(time.Duration(settingsElapsed.Load()))
		}
		scans := atomic.Int64{}
		settings.afterFlowScanForTest = func() {
			scans.Add(1)
		}
		detector := newDmcaDetector(ctx, settings, newWebStandardDetector(DefaultWebStandardSettings()))
		defer func() {
			cancel()
			<-detector.runDone
		}()
		scanInterval := settings.FlowScanInterval
		path := dmcaPath(IpProtocolUdp, 40001, 50000, false)
		detector.classify(path, []byte("x"))
		if detector.flowCount() != 1 {
			t.Fatal("the flow was not tracked")
		}

		// the scans' clock passes FlowTtl four times over
		time.Sleep(4 * (settings.FlowTtl + scanInterval))
		synctest.Wait()
		if got := scans.Load(); got == 0 || detector.flowCount() != 1 {
			t.Fatalf("%d scans left %d flows, want the held settings clock to keep the one flow", got, detector.flowCount())
		}

		settingsElapsed.Store(int64(settings.FlowTtl / 2))
		detector.touchEgress(path)
		settingsElapsed.Store(int64(settings.FlowTtl + time.Nanosecond))
		time.Sleep(scanInterval)
		synctest.Wait()
		if detector.flowCount() != 1 {
			t.Fatal("a scan reclaimed a flow refreshed within FlowTtl on the settings clock")
		}

		settingsElapsed.Store(int64(settings.FlowTtl/2 + settings.FlowTtl + time.Nanosecond))
		time.Sleep(scanInterval)
		synctest.Wait()
		if detector.flowCount() != 0 {
			t.Fatal("the scan kept a flow idle past FlowTtl on the settings clock")
		}
	})
}
