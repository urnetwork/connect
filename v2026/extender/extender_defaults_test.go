// The production defaults of the extender settings (EXTENDER.md A5, A6, A9,
// A11, A12). Every bound a prober can reach is a number in one place, so the
// numbers are pinned here rather than only in the tests that cross each bound.

package extender

import (
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Every default bound of A5, A6 and A9 has the value the section names. A
// change here is a change to what an unconfigured extender exposes.
func TestDefaultExtenderSettingsValues(t *testing.T) {
	settings := DefaultExtenderSettings()

	cases := []struct {
		name     string
		value    any
		expected any
	}{
		{name: "ReadTimeout", value: settings.ReadTimeout, expected: 30 * time.Second},
		{name: "WriteTimeout", value: settings.WriteTimeout, expected: 30 * time.Second},
		{name: "ValidFrom", value: settings.ValidFrom, expected: 180 * 24 * time.Hour},
		{name: "ValidFor", value: settings.ValidFor, expected: 180 * 24 * time.Hour},

		{name: "HeaderTimeout", value: settings.HeaderTimeout, expected: 10 * time.Second},
		{name: "QuicIdleTimeout", value: settings.QuicIdleTimeout, expected: 30 * time.Second},
		{name: "MaxConnectionCountPerSource", value: settings.MaxConnectionCountPerSource, expected: 64},
		{name: "MaxConnectionCount", value: settings.MaxConnectionCount, expected: 4096},

		{name: "ProxyMaxRequestByteCount", value: settings.ProxyMaxRequestByteCount, expected: int64(1024 * 1024)},
		{name: "ProxyMaxResponseByteCount", value: settings.ProxyMaxResponseByteCount, expected: int64(8 * 1024 * 1024)},
		{name: "ProxyMaxConnectionCountPerSource", value: settings.ProxyMaxConnectionCountPerSource, expected: 8},
		{name: "ProxyMaxConnectionCount", value: settings.ProxyMaxConnectionCount, expected: 256},
		{name: "ProxyIdleTimeout", value: settings.ProxyIdleTimeout, expected: 30 * time.Second},

		{name: "DnsMaxQueryRatePerSource", value: settings.DnsMaxQueryRatePerSource, expected: float64(10)},
		{name: "DnsMaxQueryBurstPerSource", value: settings.DnsMaxQueryBurstPerSource, expected: 20},
		{name: "DnsMaxQueryRate", value: settings.DnsMaxQueryRate, expected: float64(500)},
		{name: "DnsMaxQueryBurst", value: settings.DnsMaxQueryBurst, expected: 500},
		{name: "DnsMaxResponseByteCount", value: settings.DnsMaxResponseByteCount, expected: 4096},
		{name: "DnsForwardWorkerCount", value: settings.DnsForwardWorkerCount, expected: 64},
		{name: "DnsForwardTimeout", value: settings.DnsForwardTimeout, expected: 5 * time.Second},

		{name: "NLayerMaxDepth", value: settings.NLayerMaxDepth, expected: 4},
		{name: "NLayerDialTimeout", value: settings.NLayerDialTimeout, expected: 10 * time.Second},
		{name: "NLayerHoldTimeout", value: settings.NLayerHoldTimeout, expected: 30 * time.Second},
		{name: "NLayerAttempts", value: settings.NLayerAttempts, expected: 2},
		{name: "NLayerClientHelloTimeout", value: settings.NLayerClientHelloTimeout, expected: 2 * time.Second},
		{name: "NLayerLimitedBackoff", value: settings.NLayerLimitedBackoff, expected: 30 * time.Second},

		{name: "AdmissionSubnetsPerMinute", value: settings.AdmissionSubnetsPerMinute, expected: 1000},
		{name: "AdmissionActionsPerSubnetPerMinute", value: settings.AdmissionActionsPerSubnetPerMinute, expected: 8},
		{name: "AdmissionRefusalsPerSubnetPerMinute", value: settings.AdmissionRefusalsPerSubnetPerMinute, expected: 8},
		{name: "AdmissionRetryAfterMin", value: settings.AdmissionRetryAfterMin, expected: 15 * time.Second},
		{name: "AdmissionRetryAfterMax", value: settings.AdmissionRetryAfterMax, expected: 60 * time.Second},
		{name: "AdmissionIpv4PrefixBitCount", value: settings.AdmissionIpv4PrefixBitCount, expected: 29},
		{name: "AdmissionIpv6PrefixBitCount", value: settings.AdmissionIpv6PrefixBitCount, expected: 56},
		{name: "AdmissionMinSubnetCount", value: settings.AdmissionMinSubnetCount, expected: 4096},
	}
	for _, c := range cases {
		if c.value != c.expected {
			t.Errorf("%s = %v, expected %v", c.name, c.value, c.expected)
		}
	}

	if !slices.Equal(settings.DnsTlds, []string{connect.DefaultExtenderDnsTld}) {
		t.Errorf("DnsTlds = %v, expected the connect default alone", settings.DnsTlds)
	}
	if settings.DnsPrivilegedPort {
		t.Error("DnsPrivilegedPort is set by default; only a platform that can take 53 sets it (L2)")
	}
	// an unconfigured extender forwards to its destinations (A11)
	if 0 < len(settings.NLayerHops) {
		t.Errorf("NLayerHops = %v, expected none", settings.NLayerHops)
	}
	// every source is under the limits, on the wall clock (A12)
	if 0 < len(settings.AdmissionUnlimitedSources) {
		t.Errorf("AdmissionUnlimitedSources = %v, expected none", settings.AdmissionUnlimitedSources)
	}
	if settings.AdmissionNow != nil {
		t.Error("AdmissionNow is set by default; only a test replaces the clock")
	}
}
