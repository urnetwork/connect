package main

import (
	"encoding/hex"
	"go/format"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestParseTolerant(t *testing.T) {
	fixture := strings.Join([]string{
		"# whole line comment",
		"; also a comment",
		"",
		"0.0.0.0 ads.example.com",
		"127.0.0.1 multi1.example.com multi2.example.com # inline comment",
		"tracker.example.net",
		"*.wild.example.org",
		"**.doublewild.example.org",
		"UPPER.Example.COM.",
		"bücher.example",
		"0.0.0.0 0.0.0.0",
		"1.2.3.4",
		"198.51.100.0/24",
		"2001:db8::/32",
		"bad..name",
		".leadingdot",
		"x.com:443",
		strings.Repeat("a", 64) + ".com",
		"12345",
	}, "\n")
	p := parseFeed(formatHostsOrDomains, []byte(fixture))

	wantHosts := []string{
		"ads.example.com",
		"multi1.example.com",
		"multi2.example.com",
		"tracker.example.net",
		"wild.example.org",
		"doublewild.example.org",
		"upper.example.com",
		"xn--bcher-kva.example",
	}
	if len(p.hosts) != len(wantHosts) {
		t.Fatalf("hosts = %v, want %v", p.hosts, wantHosts)
	}
	for i, want := range wantHosts {
		if p.hosts[i] != want {
			t.Errorf("host[%d] = %q, want %q", i, p.hosts[i], want)
		}
	}
	if len(p.ranges) != 3 { // 0.0.0.0/32 (hosts-style token), 1.2.3.4/32, 198.51.100.0/24
		t.Errorf("v4 ranges = %d, want 3", len(p.ranges))
	}
	if len(p.ranges6) != 1 { // 2001:db8::/32
		t.Errorf("v6 ranges = %d, want 1", len(p.ranges6))
	}
	if p.invalid != 5 { // bad..name, .leadingdot, x.com:443, 64-char label, 12345
		t.Errorf("invalid = %d, want 5", p.invalid)
	}
}

func TestParseAdblock(t *testing.T) {
	fixture := strings.Join([]string{
		"! adblock comment",
		"[Adblock Plus 2.0]",
		"# hash comment",
		"||ads.example.com^",
		"||tracker.example.net^$third-party",
		"@@||allowed.example.com^",
		"||path.example.com/banner^",
		"||*.wild.example.com^",
		"||bücher.example^",
		"plain.example.com",
	}, "\n")
	p := parseFeed(formatAdblock, []byte(fixture))

	wantHosts := []string{"ads.example.com", "xn--bcher-kva.example"}
	if len(p.hosts) != len(wantHosts) || p.hosts[0] != wantHosts[0] || p.hosts[1] != wantHosts[1] {
		t.Fatalf("hosts = %v, want %v", p.hosts, wantHosts)
	}
	if p.skipped != 5 {
		t.Errorf("skipped = %d, want 5", p.skipped)
	}
	if p.invalid != 0 {
		t.Errorf("invalid = %d, want 0", p.invalid)
	}
}

func TestNormalizeHost(t *testing.T) {
	cases := []struct {
		raw  string
		want string
		ok   bool
	}{
		{"Ads.Example.COM", "ads.example.com", true},
		{"ads.example.com.", "ads.example.com", true},
		{"  spaced.example.com  ", "spaced.example.com", true},
		{"_dmarc.example.com", "_dmarc.example.com", true},
		{"-weird-.example.com", "-weird-.example.com", true},
		{"bücher.example", "xn--bcher-kva.example", true},
		{strings.Repeat("a", 63) + ".com", strings.Repeat("a", 63) + ".com", true},
		{"", "", false},
		{".", "", false},
		{"..", "", false},
		{"a..b", "", false},
		{".a.com", "", false},
		{"a.com..", "", false},
		{strings.Repeat("a", 64) + ".com", "", false},
		{strings.Repeat("a.", 130) + "com", "", false}, // > 253
		{"1.2.3.4", "", false},
		{"12345", "", false},
		{"x.com:443", "", false},
		{"exa mple.com", "", false},
		{"got%encoded.com", "", false},
	}
	for _, tc := range cases {
		got, ok := normalizeHost(tc.raw)
		if ok != tc.ok || got != tc.want {
			t.Errorf("normalizeHost(%q) = (%q, %v), want (%q, %v)", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPublicSuffixGuard(t *testing.T) {
	rejected := []string{"com", "net", "co.uk", "github.io", "cloudfront.net", "s3.amazonaws.com", "localhost"}
	for _, name := range rejected {
		if !isPublicSuffixEntry(name) {
			t.Errorf("%q: expected public suffix rejection", name)
		}
	}
	accepted := []string{"example.com", "ads.example.co.uk", "foo.github.io", "bucket.s3.amazonaws.com", "cloudfront.net.example.org"}
	for _, name := range accepted {
		if isPublicSuffixEntry(name) {
			t.Errorf("%q: unexpected public suffix rejection", name)
		}
	}
}

func TestCompressSuffixes(t *testing.T) {
	set := map[string]bool{
		"ads.x.com":     true,
		"a.ads.x.com":   true,
		"b.a.ads.x.com": true,
		"other.com":     true,
	}
	dropped := compressSuffixes(set)
	if dropped != 2 {
		t.Fatalf("dropped = %d, want 2", dropped)
	}
	if !set["ads.x.com"] || !set["other.com"] || len(set) != 2 {
		t.Fatalf("set = %v", set)
	}

	// a chain collapses to its root entry
	chain := map[string]bool{
		"x.com":       true,
		"ads.x.com":   true,
		"a.ads.x.com": true,
	}
	dropped = compressSuffixes(chain)
	if dropped != 2 || !chain["x.com"] || len(chain) != 1 {
		t.Fatalf("chain = %v (dropped %d)", chain, dropped)
	}
}

func TestSubtractAllow(t *testing.T) {
	block := map[string]bool{
		"ads.x.com":     true,
		"sub.ads.x.com": true,
		"other.com":     true,
	}
	// exact removal only: an allow name that is a CHILD of a blocked parent
	// removes nothing (the parent still suffix-matches it at runtime) — the
	// documented limitation.
	removed := subtractAllow(block, map[string]bool{"deep.sub.ads.x.com": true})
	if removed != 0 || len(block) != 3 {
		t.Fatalf("child allow: removed %d, set %v", removed, block)
	}
	// exact removal of a parent leaves an explicitly listed child blocked
	removed = subtractAllow(block, map[string]bool{"ads.x.com": true})
	if removed != 1 || block["ads.x.com"] || !block["sub.ads.x.com"] {
		t.Fatalf("parent allow: removed %d, set %v", removed, block)
	}
}

// TestHashVectors pins hashHost to the shared golden vectors, which the
// connect runtime tests (ip_blocker_test.go TestBlockerHashVectors) also
// assert — generator/runtime agreement without a cross-package import.
func TestHashVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/hash_vectors.txt")
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	pepper := ""
	checked := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("bad vector line: %q", line)
		}
		if fields[0] == "pepper-hex" {
			raw, err := hex.DecodeString(fields[1])
			if err != nil || len(raw) != 32 {
				t.Fatalf("bad pepper: %v (len %d)", err, len(raw))
			}
			pepper = string(raw)
			continue
		}
		want, err := strconv.ParseUint(fields[1], 16, 64)
		if err != nil {
			t.Fatalf("bad key on %q: %v", line, err)
		}
		// vector names are already normalized; normalizeHost must be a no-op
		name, ok := normalizeHost(fields[0])
		if !ok || name != fields[0] {
			t.Fatalf("vector name %q not normalization-stable (got %q, %v)", fields[0], name, ok)
		}
		if got := hashHost(pepper, name); got != want {
			t.Errorf("hashHost(%s) = %016x, want %016x", name, got, want)
		}
		checked += 1
	}
	if pepper == "" || checked == 0 {
		t.Fatalf("vectors file missing pepper or rows (checked %d)", checked)
	}
}

func TestRangeGuards(t *testing.T) {
	var p parsed
	// broader than /8 is poison
	if !routePrefixString(&p, "1.0.0.0/7") {
		t.Fatalf("cidr not recognized")
	}
	if len(p.ranges) != 0 || p.invalid != 1 {
		t.Fatalf("poison /7 accepted: %+v", p)
	}
	if !routePrefixString(&p, "1.0.0.0/8") || len(p.ranges) != 1 {
		t.Fatalf("/8 rejected: %+v", p)
	}
	// v6: broader than /16 is poison
	if !routePrefixString(&p, "2001::/15") || len(p.ranges6) != 0 || p.invalid != 2 {
		t.Fatalf("poison v6 /15 accepted: %+v", p)
	}
	if !routePrefixString(&p, "2001::/16") || len(p.ranges6) != 1 {
		t.Fatalf("v6 /16 rejected: %+v", p)
	}
	// not a cidr
	if routePrefixString(&p, "ads.example.com") {
		t.Fatalf("hostname routed as cidr")
	}
}

func TestMergeSubtract(t *testing.T) {
	merged := mergeRanges([]iprange{{3, 10}, {1, 5}, {12, 12}, {11, 11}})
	if len(merged) != 1 || merged[0].lo != 1 || merged[0].hi != 12 {
		t.Fatalf("merged = %+v", merged)
	}
	final := subtractAll(merged, []iprange{{5, 6}})
	if len(final) != 2 || final[0] != (iprange{1, 4}) || final[1] != (iprange{7, 12}) {
		t.Fatalf("final = %+v", final)
	}

	a := netip.MustParseAddr
	merged6 := mergeRanges6([]ip6range{
		{a("2001:db8::10"), a("2001:db8::20")},
		{a("2001:db8::"), a("2001:db8::15")},
	})
	if len(merged6) != 1 || merged6[0].lo != a("2001:db8::") || merged6[0].hi != a("2001:db8::20") {
		t.Fatalf("merged6 = %+v", merged6)
	}
	final6 := subtractAll6(merged6, []ip6range{{a("2001:db8::5"), a("2001:db8::6")}})
	if len(final6) != 2 || final6[0].hi != a("2001:db8::4") || final6[1].lo != a("2001:db8::7") {
		t.Fatalf("final6 = %+v", final6)
	}
}

func TestMakePepper(t *testing.T) {
	p1, err := makePepper("")
	if err != nil || len(p1) != 32 {
		t.Fatalf("random pepper: %v (len %d)", err, len(p1))
	}
	p2, _ := makePepper("")
	if p1 == p2 {
		t.Fatalf("two random peppers are equal")
	}
	fixed := strings.Repeat("ab", 32)
	p3, err := makePepper(fixed)
	if err != nil || hex.EncodeToString([]byte(p3)) != fixed {
		t.Fatalf("fixed pepper roundtrip: %v", err)
	}
	if _, err := makePepper("zz"); err == nil {
		t.Fatalf("bad hex accepted")
	}
	if _, err := makePepper("abcd"); err == nil {
		t.Fatalf("short pepper accepted")
	}
}

func TestEmitDeterministicAndOpaque(t *testing.T) {
	pepper, _ := makePepper(strings.Repeat("ab", 32))
	names := map[string]bool{
		"ads.example.com":     true,
		"tracker.example.net": true,
	}
	keys := hashHosts(pepper, names)
	if len(keys) != 2 {
		t.Fatalf("keys = %d", len(keys))
	}
	ranges := []iprange{{0x01020304, 0x010203ff}}
	ranges6 := []ip6range{{netip.MustParseAddr("2001:db8::"), netip.MustParseAddr("2001:db8::ff")}}

	out1 := emit("standard", pepper, keys, ranges, ranges6, feeds[:2])
	out2 := emit("standard", pepper, keys, ranges, ranges6, feeds[:2])
	if string(out1) != string(out2) {
		t.Fatalf("emit is not deterministic")
	}
	if !strings.Contains(string(out1), "DO NOT EDIT") {
		t.Fatalf("missing generated marker")
	}
	// the obfuscation property: no plain text entry appears in the output
	for name := range names {
		if strings.Contains(string(out1), name) {
			t.Fatalf("plain text entry %q leaked into the generated source", name)
		}
	}
	if _, err := format.Source(out1); err != nil {
		t.Fatalf("emit output does not gofmt: %v", err)
	}
	// counts are embedded
	if !strings.Contains(string(out1), "const blockerBlockedHostCount = 2") {
		t.Fatalf("host count missing")
	}
	if !strings.Contains(string(out1), "const blockerBlockedPrefixCount = 1") {
		t.Fatalf("v4 count missing")
	}
	if !strings.Contains(string(out1), "const blockerBlockedPrefix6Count = 1") {
		t.Fatalf("v6 count missing")
	}
}

func TestResolverExclusions(t *testing.T) {
	names, addrs := resolverExclusions()
	// the default settings carry ip-addressed doh urls; those must surface as
	// addresses to subtract. hostname counts depend on defaults — just assert
	// the call works and returns unmapped addrs.
	if len(addrs) == 0 {
		t.Fatalf("expected at least one resolver address from defaults")
	}
	for _, a := range addrs {
		if a.Is4In6() {
			t.Errorf("resolver addr %s not unmapped", a)
		}
	}
	_ = names
}
