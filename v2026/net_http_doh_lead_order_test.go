package connect

import (
	"slices"
	"testing"
)

// The lead servers' parts (`DohSettings.NamedRemoteDohUrls`,
// `DohSettings.DohServerLeadReserve`): which servers lead, the order a query
// launches them in, and the share of the request slots the others may hold.
// net_http_doh_lead_test.go runs them through the internal resolver.

// The lead servers are the named ones and the regional presets of the ingress
// country, and the tunnel's settings have none.
func TestDohLeadUrls(t *testing.T) {
	namedDohUrl := "https://192.0.2.53/dns-query"
	presetsIpv4, presetsIpv6 := RegionalControlDohUrls("cn")
	presets := slices.Concat(presetsIpv4, presetsIpv6)
	if len(presets) == 0 {
		t.Fatal("no cn presets")
	}
	expectLead := func(name string, settings *DohSettings, expected []string) {
		leadDohUrls := dohLeadUrls(settings)
		if len(leadDohUrls) != len(expected) {
			t.Errorf("%s: %d lead servers, expected %d", name, len(leadDohUrls), len(expected))
		}
		for _, dohUrl := range expected {
			if !leadDohUrls[dohUrl] {
				t.Errorf("%s: %s does not lead", name, dohUrl)
			}
		}
	}
	expectLead("tunnel", DefaultDohSettings(), nil)
	expectLead("control without named servers", ControlDohSettings(nil, nil), nil)
	expectLead("control", ControlDohSettings([]string{namedDohUrl}, nil), []string{namedDohUrl})
	expectLead("cn", IngressDohSettings("cn", nil, nil), presets)
	expectLead("cn with a named server", IngressDohSettings("cn", []string{namedDohUrl}, nil), slices.Concat([]string{namedDohUrl}, presets))
	expectLead("a country without presets", IngressDohSettings("us", nil, nil), nil)
	expectLead("built-in servers only", builtInDohServerSettings(ControlDohSettings([]string{namedDohUrl}, nil)), nil)
	expectLead("built-in servers only in cn", builtInDohServerSettings(IngressDohSettings("cn", []string{namedDohUrl}, nil)), presets)
}

// A query launches the lead servers before every other server, whatever the
// draw, with stats as a cache has them and without, as a one-shot query has
// them. Each group keeps its own order.
func TestDohLaunchOrderPutsTheLeadServersFirst(t *testing.T) {
	namedDohUrl := "https://192.0.2.53/dns-query"
	settings := ControlDohSettings([]string{namedDohUrl}, nil)
	dohUrls := remoteDohUrls(settings, 0)
	leadDohUrls := dohLeadUrls(settings)
	seededStats := newServerStats()
	seededStats.seed(settings.ServerStatsSeed)
	for _, stats := range []*serverStats{nil, newServerStats(), seededStats} {
		client := &dohClient{stats: stats, leadDohUrls: leadDohUrls}
		for range 200 {
			ordered := client.launchOrder(dohUrls)
			if !slices.Equal(slices.Sorted(slices.Values(ordered)), slices.Sorted(slices.Values(dohUrls))) {
				t.Fatalf("the launch order %v is not the servers %v", ordered, dohUrls)
			}
			if ordered[0] != namedDohUrl {
				t.Fatalf("the launch order %v does not start with the named server", ordered)
			}
		}
	}
	// without lead servers the order is the weighted order alone
	client := &dohClient{stats: newServerStats()}
	ordered := client.launchOrder(dohUrls)
	if !slices.Equal(slices.Sorted(slices.Values(ordered)), slices.Sorted(slices.Values(dohUrls))) {
		t.Fatalf("the launch order %v is not the servers %v", ordered, dohUrls)
	}
}

// The servers outside the lead tier may hold all the request slots but the
// lead reserve, and at least one. Without a lead server, or with no reserve,
// they share every slot as before.
func TestDohFollowerSemLeavesTheLeadReserve(t *testing.T) {
	leadDohUrls := map[string]bool{"https://192.0.2.53/dns-query": true}
	cases := []struct {
		httpConcurrency int
		leadReserve     int
		leadDohUrls     map[string]bool
		followerCount   int
	}{
		{httpConcurrency: 4, leadReserve: 1, leadDohUrls: leadDohUrls, followerCount: 3},
		{httpConcurrency: 16, leadReserve: 1, leadDohUrls: leadDohUrls, followerCount: 15},
		// at least one slot stays open to the others
		{httpConcurrency: 4, leadReserve: 9, leadDohUrls: leadDohUrls, followerCount: 1},
		{httpConcurrency: 1, leadReserve: 1, leadDohUrls: leadDohUrls, followerCount: 0},
		{httpConcurrency: 4, leadReserve: 0, leadDohUrls: leadDohUrls, followerCount: 0},
		{httpConcurrency: 4, leadReserve: -1, leadDohUrls: leadDohUrls, followerCount: 0},
		{httpConcurrency: 4, leadReserve: 1, leadDohUrls: nil, followerCount: 0},
	}
	for _, c := range cases {
		followerSem := newDohFollowerSem(c.httpConcurrency, c.leadReserve, c.leadDohUrls)
		if c.followerCount == 0 {
			if followerSem != nil {
				t.Errorf("%d slots, reserve %d, %d lead servers: a share of %d, expected none", c.httpConcurrency, c.leadReserve, len(c.leadDohUrls), cap(followerSem))
			}
			continue
		}
		if followerSem == nil || cap(followerSem) != c.followerCount {
			t.Errorf("%d slots, reserve %d: a share of %d, expected %d", c.httpConcurrency, c.leadReserve, cap(followerSem), c.followerCount)
		}
	}
}

// The control names' cache keeps its four request slots and leaves the named
// server one of them, its remote and local clients sharing the share as they
// share the slots; the tunnel's cache has no lead server and no share.
func TestDohCacheLeadReserveIsTheControlNamesOnly(t *testing.T) {
	namedDohUrl := "https://192.0.2.53/dns-query"
	control := NewDohCache(internalDohSettings(ControlDohSettings([]string{namedDohUrl}, nil), []string{"space.example"}))
	defer control.Close()
	if cap(control.remoteClient.httpSem) != 4 {
		t.Fatalf("the control names' cache has %d request slots, expected 4", cap(control.remoteClient.httpSem))
	}
	followerSem := control.remoteClient.followerSem
	if followerSem == nil || cap(followerSem) != 4-DefaultDohSettings().DohServerLeadReserve {
		t.Fatalf("the control names' cache leaves the lead servers %d of its 4 slots, expected %d", 4-cap(followerSem), DefaultDohSettings().DohServerLeadReserve)
	}
	if control.localClient.followerSem != followerSem || !control.remoteClient.leadDohUrls[namedDohUrl] {
		t.Fatal("the remote and local clients do not share the lead servers' reserve")
	}

	tunnel := NewDohCache(DefaultDohSettings())
	defer tunnel.Close()
	if tunnel.remoteClient.followerSem != nil || tunnel.remoteClient.leadDohUrls != nil {
		t.Fatal("the tunnel's cache has lead servers")
	}
}
