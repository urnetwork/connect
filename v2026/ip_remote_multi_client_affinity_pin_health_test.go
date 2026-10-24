// Fresh pin placement exempts affinity age, never provider health. Existing
// pinned tuple owners are a different lifecycle and must remain untouched.
package connect

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"
)

func testAffinityFreshPinHealth(t *testing.T, kind string) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		for _, version := range []int{4, 6} {
			for _, state := range []string{
				"healthy", "warning", "unhealthy", "unknown", "quarantine", "canceled", "terminal",
				"closed", "route-gone", "warning-at-adoption", "unhealthy-at-adoption",
				"closed-at-adoption", "route-gone-at-adoption",
			} {
				caps := []time.Duration{time.Minute, 0}
				if kind == "dns-address" || kind == "dns-domain" {
					// Publication at cap0 intentionally creates no unused hints.
					// Here an actual young answer must survive age, not health.
					caps = caps[:1]
				}
				for _, cap := range caps {
					parent := affinityTimeTestParent(t)
					pin := flowPin{site: true}
					oldVersion := version
					if kind == "app" {
						pin = flowPin{appId: "affinity-test"}
						oldVersion = 10 - version // force cross-family canonical lookup
					}
					old, _, _ := parent.sendUpdate(affinityTimeTestPath(oldVersion, 44000), pin)
					donor := &multiClientChannel{
						ctx: parent.ctx, settings: parent.settings, healthy: true,
						args: &multiClientChannelArgs{DestinationStats: DestinationStats{IpFamily: IpFamilyDualstack}},
					}
					old.selectAffinityClient(donor)
					old.receivedInbound.Store(true)
					parent.bindClientFlow(old, donor)
					path := affinityTimeTestPath(version, 44001)
					if kind == "dns-address" || kind == "dns-domain" {
						address := netip.MustParseAddr("203.0.113.73")
						if version == 6 {
							address = netip.MustParseAddr("2001:db8:3::73")
						}
						domain := "pin.affinity.example"
						if !parent.bindDnsResultToExit(old.ipPath, domain, []netip.Addr{address}) {
							t.Fatal("healthy pin fixture failed to record its actual DNS answer")
						}
						path.DestinationIp = address.AsSlice()
						parent.config.Store(&multiClientConfig{serverNameLookup: stubServerNameLookup{names: []string{domain}}})
						if kind == "dns-domain" {
							clear(parent.dnsAddressExitHints)
						}
					}
					time.Sleep(time.Minute)
					if cap == 0 {
						settings := ReliabilitySettingsFrom(parent.settings)
						settings.AffinityMaxTime = 0
						parent.SetReliabilitySettings(settings)
					}
					var mutateCarrier func()
					var joinCarrier func() error
					if state == "closed" || state == "route-gone" || state == "closed-at-adoption" || state == "route-gone-at-adoption" {
						f := newGroupDispositionQueueFixture(t, nil, nil)
						donor.client = f.client
						joinCarrier = func() error { return f.closeClient(context.Background()) }
						if !donor.hasActiveTransport() {
							t.Fatal("pin carrier positive control is not active")
						}
						mutateCarrier = func() {
							if state == "closed" || state == "closed-at-adoption" {
								f.client.Close()
							} else {
								f.client.RouteManager().RemoveTransport(f.transport)
							}
						}
					}
					atAdoption := false
					switch state {
					case "warning":
						donor.warning = true
					case "unhealthy":
						donor.healthy, donor.lastUnhealthyTime = false, time.Now()
					case "unknown":
						donor.healthy = false
					case "quarantine":
						donor.quarantined = true
					case "canceled":
						ctx, cancel := context.WithCancel(parent.ctx)
						donor.ctx = ctx
						cancel()
					case "terminal":
						donor.endErr = errors.New("synthetic terminal pinned provider")
					case "closed", "route-gone":
						mutateCarrier()
					case "warning-at-adoption", "unhealthy-at-adoption", "closed-at-adoption", "route-gone-at-adoption":
						parent.beforeAffinityAdoptionForTest = func(client *multiClientChannel) {
							atAdoption = true
							if mutateCarrier != nil {
								mutateCarrier()
								return
							}
							client.stateLock.Lock()
							defer client.stateLock.Unlock()
							if state == "warning-at-adoption" {
								client.warning = true
							} else {
								client.healthy = false
							}
						}
					}
					fresh, _, client := parent.sendUpdate(path, pin)
					if (client == donor) != (state == "healthy") {
						t.Fatalf("fresh %s pin v%d cap=%v state=%s inherited=%v", kind, version, cap, state, client == donor)
					}
					if fresh == old || !fresh.pinned || donor.args.FixedDestination {
						t.Fatal("pin fixture bypassed ordinary target-family placement")
					}
					if (state == "warning-at-adoption" || state == "unhealthy-at-adoption" || state == "closed-at-adoption" || state == "route-gone-at-adoption") && !atAdoption {
						t.Fatal("fresh pin skipped the final adoption boundary")
					}
					data := *old.ipPath
					data.Syn = false
					current, _, incumbent := parent.sendUpdate(&data, pin)
					if current != old || incumbent != donor || old.IsDone() || !old.pinned || !old.receivedInbound.Load() {
						t.Fatal("fresh-pin health policy migrated an established pinned tuple")
					}
					if joinCarrier != nil {
						if err := joinCarrier(); err != nil {
							t.Fatalf("join pin carrier before next matrix row: %v", err)
						}
					}
				}
			}
		}
	})
}

func TestAffinityMaxTimeFreshHostPinsRequireHealthyProvider(t *testing.T) {
	assertMessagePoolOwnership(t)
	testAffinityFreshPinHealth(t, "host")
}

func TestAffinityMaxTimeFreshAppPinsRequireHealthyProvider(t *testing.T) {
	assertMessagePoolOwnership(t)
	testAffinityFreshPinHealth(t, "app")
}

func TestAffinityMaxTimeFreshDnsPinsRequireHealthyProvider(t *testing.T) {
	assertMessagePoolOwnership(t)
	testAffinityFreshPinHealth(t, "dns-address")
	testAffinityFreshPinHealth(t, "dns-domain")
}
