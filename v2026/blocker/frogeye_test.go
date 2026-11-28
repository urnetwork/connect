package main

import (
	"strings"
	"testing"
	"time"
)

func TestFrogeyeIntegrity(t *testing.T) {
	now := time.Date(2026, 11, 13, 6, 0, 0, 0, time.UTC)
	const fixture = `# First-party trackers host list
# Variant: first-party trackers
# List generation date: 2026-11-09T00:00:50+01:00
# Oldest record: 2026-11-08T13:30:11+01:00
# Number of source subdomains: 3219361
# Number of source DNS records: 2843950
# Output rules: hostnames: 2

0.0.0.0 ads.example.com
0.0.0.0 tracker.example.net
`
	for _, variant := range []string{"first-party trackers", "multi-party trackers"} {
		data := strings.ReplaceAll(fixture, "# Variant: first-party trackers", "# Variant: "+variant)
		p := parseFrogeye([]byte(data), variant, now)
		if p.err != nil || len(p.hosts) != 2 || len(p.ranges) != 0 || len(p.ranges6) != 0 {
			t.Fatalf("valid %s response: %+v", variant, p)
		}
	}
	cases := map[string]string{
		"truncated payload":       strings.ReplaceAll(fixture, "0.0.0.0 tracker.example.net\n", ""),
		"duplicate domain":        strings.ReplaceAll(fixture, "tracker.example.net", "ads.example.com"),
		"extra domain":            fixture + "0.0.0.0 another.example.org\n",
		"wrong count":             strings.ReplaceAll(fixture, "hostnames: 2", "hostnames: 3"),
		"malformed count":         strings.ReplaceAll(fixture, "hostnames: 2", "zones: 2"),
		"duplicate metadata":      fixture + "# Output rules: hostnames: 2\n",
		"missing metadata":        strings.ReplaceAll(fixture, "# Number of source DNS records: 2843950\n", ""),
		"wrong variant":           strings.ReplaceAll(fixture, "# Variant: first-party", "# Variant: multi-party"),
		"missing title":           strings.ReplaceAll(fixture, "# First-party trackers host list\n", ""),
		"duplicate title":         fixture + "# First-party trackers host list\n",
		"stale generation":        strings.ReplaceAll(fixture, "2026-11-09T00:00:50+01:00", "2026-10-29T00:00:50+01:00"),
		"stale records":           strings.ReplaceAll(fixture, "2026-11-08T13:30:11+01:00", "2026-10-29T13:30:11+01:00"),
		"future generation":       strings.ReplaceAll(fixture, "2026-11-09T00:00:50+01:00", "2026-11-15T00:00:50+01:00"),
		"future records":          strings.ReplaceAll(fixture, "2026-11-08T13:30:11+01:00", "2026-11-15T13:30:11+01:00"),
		"reversed dates":          strings.ReplaceAll(fixture, "2026-11-08T13:30:11+01:00", "2026-11-10T13:30:11+01:00"),
		"malformed date":          strings.ReplaceAll(fixture, "2026-11-09T00:00:50+01:00", "yesterday"),
		"low subdomains":          strings.ReplaceAll(fixture, "3219361", "999999"),
		"low DNS":                 strings.ReplaceAll(fixture, "2843950", "999999"),
		"malformed source volume": strings.ReplaceAll(fixture, "2843950", "many"),
		"bare domain":             strings.ReplaceAll(fixture, "0.0.0.0 ads", "ads"),
		"non-null target":         strings.ReplaceAll(fixture, "0.0.0.0 ads", "1.2.3.4 ads"),
		"IP payload":              strings.ReplaceAll(fixture, "ads.example.com", "8.8.8.8"),
		"invalid domain":          strings.ReplaceAll(fixture, "ads.example.com", "bad..example.com"),
		"noncanonical domain":     strings.ReplaceAll(fixture, "ads.example.com", "ADS.example.com"),
		"inline junk":             strings.ReplaceAll(fixture, "ads.example.com", "ads.example.com extra.example.org"),
		"scanner error":           fixture + "#" + strings.Repeat("x", 70*1024),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if p := parseFrogeye([]byte(data), "first-party trackers", now); p.err == nil {
				t.Fatal("accepted corrupt or unhealthy feed")
			}
		})
	}
}

func TestFrogeyeResponseFailsClosed(t *testing.T) {
	for _, f := range feeds {
		if f.frogeyeVariant == "" {
			continue
		}
		p := parseResponse(f, []byte("0.0.0.0 ads.example.com\n"))
		if p.err == nil || !p.contentAvailable || p.f != f {
			t.Fatalf("response did not preserve failure and provenance: %+v", p)
		}
	}
}
