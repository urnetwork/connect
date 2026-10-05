package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"go/format"
	"io"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A trimmed RADb answer to "-i origin AS32934", with one object of every kind
// the filter must tell apart: Meta's own RADB and RIPE registrations
// (duplicated across both, contained, adjacent, a comma list and a continued
// mnt-by, lower case), RADb's RPKI conversions, another
// registry's copy, third-party registrations, a wrong origin, a prefix with
// host bits, a family mismatch, server messages, comments and a non-route
// object.
const metaTestResponse = `% Information related to AS32934 (synthetic fixture)
# comment line

route:          31.13.64.0/18
descr:          Facebook, Inc.
origin:         AS32934
mnt-by:         MAINT-AS32934
changed:        phoose@fb.com 20111006  #18:05:29Z
source:         RADB
last-modified:  2023-11-13T15:41:43Z
rpki-ov-state:  valid

route:          31.13.64.0/19
descr:          Facebook, Inc.
origin:         AS32934
mnt-by:         MAINT-AS32934
source:         RADB

route:          157.240.0.0/16
descr:          Facebook, Inc.
origin:         AS32934
mnt-by:         MAINT-AS32934
source:         RADB

route:          157.240.0.0/16
descr:          RPKI ROA for 157.240.0.0/16 / AS32934
remarks:        This AS32934 route object represents routing data retrieved
                from the RPKI. This route object is the result of an automated
                RPKI-to-IRR conversion process performed by IRRd.
max-length:     24
origin:         AS32934
source:         RPKI  # Trust Anchor: ripe

route:          57.141.0.0/24
descr:          Meta Route
origin:         AS32934
mnt-by:         fb-neteng
mnt-by:         facebook-neteng
mnt-by:         meta-mnt
source:         RIPE

route:          57.141.0.0/24
descr:          Facebook, Inc.
origin:         AS32934
mnt-by:         MAINT-AS32934
source:         RADB

route:          57.141.1.0/24
descr:          Facebook, Inc.
origin:         as32934
mnt-by:         MNT-OTHER, maint-as32934
source:         radb

route:          69.63.176.0/20
descr:          Facebook
origin:         AS32934
mnt-by:         MNT-THEFA-3
source:         ARIN

route:          45.64.40.0/22
descr:          Facebook, Inc.
origin:         AS32934
mnt-by:         MAINT-AS32934
source:         RADB

route:          192.0.2.0/24
descr:          wrong origin
origin:         AS64496
mnt-by:         MAINT-AS32934
source:         RADB

route:          198.51.100.7/24
descr:          host bits set
origin:         AS32934
mnt-by:         MAINT-AS32934
source:         RADB

route:          203.0.113.0/24
descr:          ISP-hosted cache
origin:         AS32934
mnt-by:         EPOLUDNIE-MNT
source:         RIPE

route:          185.60.216.0/22
descr:          Facebook, Inc.
origin:         AS32934
mnt-by:         MNT-OTHER,
                MAINT-AS32934
source:         RADB

route6:         2a03:2880::/32
descr:          Facebook, Inc.
origin:         AS32934
mnt-by:         MAINT-AS32934
source:         RADB

route6:         2a03:2881::/32
descr:          Facebook, Inc.
origin:         AS32934
mnt-by:         MAINT-AS32934
source:         RADB

route6:         2a10:f781:10:cee0::/64
descr:          EPIX-LORA-FACEBOOK
origin:         AS32934
mnt-by:         EPOLUDNIE-MNT
source:         RIPE

route6:         157.240.0.0/16
descr:          family mismatch
origin:         AS32934
mnt-by:         MAINT-AS32934
source:         RADB

route6:         2a03:83e0::/32
origin:         AS32934
mnt-by:         facebook-ene
mnt-by:         facebook-neteng
source:         RIPE

as-set:         AS-FACEBOOK
mnt-by:         MAINT-AS32934
source:         RADB

`

// metaTestSource bounds the fixture exactly: every floor and cap is met with
// nothing to spare, so one step either way trips it.
func metaTestSource() *metaSource {
	source := metaRadb
	source.minRoutes = 7
	source.minRoutes6 = 3
	source.minPrefixes = 5
	source.minPrefixes6 = 2
	source.minCoverage = 84_480
	source.maxCoverage = 84_480
	source.maxCoverage48 = 196_608
	return &source
}

func mustPrefixes(t *testing.T, values ...string) []netip.Prefix {
	t.Helper()
	prefixes := []netip.Prefix{}
	for _, value := range values {
		prefixes = append(prefixes, netip.MustParsePrefix(value))
	}
	return prefixes
}

func TestMetaAcceptsOnlyMetaMaintainedRegistrations(t *testing.T) {
	objects, messages := parseRpsl([]byte(metaTestResponse))
	if len(messages) != 1 || !strings.HasPrefix(messages[0], "Information related to AS32934") {
		t.Fatalf("server messages = %q, want the one fixture message", messages)
	}
	routes, report, problems := metaTestSource().acceptObjects(objects)
	if len(problems) != 0 {
		t.Fatalf("problems = %q, want none", problems)
	}
	// whois servers may end lines with CRLF
	crlfObjects, _ := parseRpsl([]byte(strings.ReplaceAll(metaTestResponse, "\n", "\r\n")))
	if crlfRoutes, crlfReport, _ := metaTestSource().acceptObjects(crlfObjects); !slices.Equal(crlfRoutes, routes) || crlfReport != report {
		t.Fatalf("CRLF answer accepted %v (%+v), want %v (%+v)", crlfRoutes, crlfReport, routes, report)
	}
	want := mustPrefixes(t,
		"31.13.64.0/18",
		"31.13.64.0/19",
		"157.240.0.0/16",
		"57.141.0.0/24",
		"57.141.0.0/24",
		"57.141.1.0/24",
		"45.64.40.0/22",
		"185.60.216.0/22",
		"2a03:2880::/32",
		"2a03:2881::/32",
		"2a03:83e0::/32",
	)
	if !slices.Equal(routes, want) {
		t.Fatalf("accepted = %v, want %v", routes, want)
	}
	wantReport := metaReport{
		objects:            19,
		routes:             13,
		route6s:            5,
		accepted:           11,
		rejectedSource:     2,
		rejectedMaintainer: 2,
		rejectedOrigin:     1,
		rejectedBad:        2,
	}
	if report != wantReport {
		t.Fatalf("report = %+v, want %+v", report, wantReport)
	}
}

func TestMetaSnapshotCollapsesSortedAndDeduplicated(t *testing.T) {
	snapshot, _, problems := metaTestSource().parseMetaSnapshot([]byte(metaTestResponse))
	if len(problems) != 0 {
		t.Fatalf("problems = %q, want none", problems)
	}
	want := mustPrefixes(t,
		"31.13.64.0/18",
		"45.64.40.0/22",
		"57.141.0.0/23",
		"157.240.0.0/16",
		"185.60.216.0/22",
		"2a03:2880::/31",
		"2a03:83e0::/32",
	)
	if !slices.Equal(snapshot.prefixes, want) {
		t.Fatalf("prefixes = %v, want %v", snapshot.prefixes, want)
	}
	// the duplicate 57.141.0.0/24 counts once
	if snapshot.routeCount != 7 || snapshot.route6Count != 3 {
		t.Fatalf("unique registrations = %d IPv4 / %d IPv6, want 7 / 3", snapshot.routeCount, snapshot.route6Count)
	}
}

func TestCollapsePrefixes(t *testing.T) {
	for _, test := range []struct {
		name string
		in   []string
		want []string
	}{
		{name: "empty"},
		{name: "duplicates and contained", in: []string{"157.240.26.0/24", "157.240.0.0/16", "157.240.0.0/16", "157.240.0.0/17"}, want: []string{"157.240.0.0/16"}},
		{name: "siblings merge", in: []string{"57.141.1.0/24", "57.141.0.0/24"}, want: []string{"57.141.0.0/23"}},
		{name: "sibling chain", in: []string{"10.0.3.0/24", "10.0.0.0/24", "10.0.2.0/24", "10.0.1.0/24"}, want: []string{"10.0.0.0/22"}},
		{name: "partial overlap", in: []string{"10.0.1.0/24", "10.0.0.0/23", "10.0.1.128/25"}, want: []string{"10.0.0.0/23"}},
		{name: "adjacent but unaligned stay apart", in: []string{"10.0.1.0/24", "10.0.2.0/24"}, want: []string{"10.0.1.0/24", "10.0.2.0/24"}},
		{name: "gap stays", in: []string{"10.0.0.0/24", "10.0.2.0/24"}, want: []string{"10.0.0.0/24", "10.0.2.0/24"}},
		// Meta's /24 registrations of 57.141.0.0 - 57.141.25.255
		{name: "run of registrations", in: func() []string {
			values := []string{}
			for i := 25; 0 <= i; i-- {
				values = append(values, "57.141."+strconv.Itoa(i)+".0/24")
			}
			return values
		}(), want: []string{"57.141.0.0/20", "57.141.16.0/21", "57.141.24.0/23"}},
		{name: "families sorted apart", in: []string{"2a03:2881::/32", "157.240.0.0/16", "2a03:2880::/32", "31.13.64.0/18"}, want: []string{"31.13.64.0/18", "157.240.0.0/16", "2a03:2880::/31"}},
		{name: "top of IPv4", in: []string{"255.255.255.254/31", "255.255.255.0/25", "255.255.255.128/25"}, want: []string{"255.255.255.0/24"}},
		{name: "top of IPv6", in: []string{"ffff:ffff:ffff:ffff:ffff:ffff:ffff:fffe/127", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ff00/120"}, want: []string{"ffff:ffff:ffff:ffff:ffff:ffff:ffff:ff00/120"}},
		{name: "IPv4 end does not merge into IPv6", in: []string{"255.255.255.255/32", "::/128"}, want: []string{"255.255.255.255/32", "::/128"}},
	} {
		if got, want := collapsePrefixes(mustPrefixes(t, test.in...)), mustPrefixes(t, test.want...); !slices.Equal(got, want) {
			t.Errorf("%s: collapse = %v, want %v", test.name, got, want)
		}
	}
}

// Against brute force: the collapsed prefixes cover exactly the addresses of
// the input, are sorted and disjoint, and no two are siblings, which makes
// the cover the fewest prefixes possible.
func TestCollapsePrefixesMatchesUnion(t *testing.T) {
	random := rand.New(rand.NewPCG(32934, 5222))
	for _, base := range []netip.Prefix{netip.MustParsePrefix("10.0.0.0/22"), netip.MustParsePrefix("2001:db8::/118")} {
		hostBits := base.Addr().BitLen() - base.Bits()
		for round := 0; round < 300; round++ {
			prefixes := []netip.Prefix{}
			for i := 0; i < 1+random.IntN(12); i++ {
				address := base.Addr()
				for offset := random.IntN(1 << hostBits); 0 < offset; offset-- {
					address = address.Next()
				}
				prefixes = append(prefixes, netip.PrefixFrom(address, base.Bits()+random.IntN(hostBits+1)).Masked())
			}
			collapsed := collapsePrefixes(prefixes)
			covered := func(set []netip.Prefix, address netip.Addr) bool {
				for _, prefix := range set {
					if prefix.Contains(address) {
						return true
					}
				}
				return false
			}
			for address := base.Addr(); base.Contains(address); address = address.Next() {
				if covered(prefixes, address) != covered(collapsed, address) {
					t.Fatalf("collapse of %v = %v differs at %s", prefixes, collapsed, address)
				}
			}
			for i := 1; i < len(collapsed); i++ {
				previous, next := collapsed[i-1], collapsed[i]
				if prefixLast(previous).Compare(next.Addr()) >= 0 {
					t.Fatalf("collapse of %v = %v is not sorted and disjoint", prefixes, collapsed)
				}
				if previous.Bits() == next.Bits() &&
					netip.PrefixFrom(previous.Addr(), previous.Bits()-1).Masked() == netip.PrefixFrom(next.Addr(), next.Bits()-1).Masked() {
					t.Fatalf("collapse of %v = %v keeps siblings %s and %s", prefixes, collapsed, previous, next)
				}
			}
		}
	}
}

// metaTestObjects splits the fixture into its objects.
func metaTestObjects() []string {
	objects := []string{}
	for _, object := range strings.Split(metaTestResponse, "\n\n") {
		if object = strings.TrimSpace(object); object != "" {
			objects = append(objects, object)
		}
	}
	return objects
}

// metaTestAnswer is a whois answer holding objects.
func metaTestAnswer(objects ...string) []byte {
	var answer bytes.Buffer
	for _, object := range objects {
		answer.WriteString(object)
		answer.WriteString("\n\n")
	}
	return answer.Bytes()
}

func metaTestEmit(t *testing.T, objects []string) []byte {
	t.Helper()
	source := metaTestSource()
	// room for an added registration
	source.maxCoverage *= 2
	source.maxCoverage48 *= 2
	snapshot, _, problems := source.parseMetaSnapshot(metaTestAnswer(objects...))
	if len(problems) != 0 {
		t.Fatalf("problems = %q, want none", problems)
	}
	return source.emitMeta(snapshot)
}

// The output depends only on the set of Meta's registrations: not on the
// order of the answer, duplicate objects, volatile attributes, or third-party
// objects. A new registration changes it.
func TestMetaSnapshotDeterministic(t *testing.T) {
	objects := metaTestObjects()
	baseline := metaTestEmit(t, objects)

	formatted, err := format.Source(baseline)
	if err != nil || !bytes.Equal(formatted, baseline) {
		t.Fatalf("emitted source is not gofmt stable: %v", err)
	}

	shuffled := slices.Clone(objects)
	slices.Reverse(shuffled)
	for i, object := range shuffled {
		object = strings.ReplaceAll(object, "2023-11-13T15:41:43Z", "2026-10-05T00:00:00Z")
		object = strings.ReplaceAll(object, "rpki-ov-state:  valid", "rpki-ov-state:  not_found")
		object = strings.ReplaceAll(object, "Facebook, Inc.", "Meta Platforms, Inc.")
		shuffled[i] = object
	}
	shuffled = append(shuffled, objects[2], objects[5], `route:          198.18.0.0/15
descr:          another third party
origin:         AS32934
mnt-by:         MNT-ELSEWHERE
source:         RIPE`)
	if varied := metaTestEmit(t, shuffled); !bytes.Equal(varied, baseline) {
		t.Fatalf("reordered answer changed the output:\n%s\nwant:\n%s", varied, baseline)
	}

	added := append(slices.Clone(objects), `route:          57.141.2.0/24
origin:         AS32934
mnt-by:         MAINT-AS32934
source:         RADB`)
	changed := metaTestEmit(t, added)
	if bytes.Equal(changed, baseline) || !bytes.Contains(changed, []byte(`netip.MustParsePrefix("57.141.2.0/24")`)) {
		t.Fatalf("a new registration did not change the output:\n%s", changed)
	}
}

// Each floor, cap and anchor aborts at one step past the bound; a dead or
// poisoned answer aborts with a reason.
func TestMetaSnapshotBounds(t *testing.T) {
	if _, _, problems := metaTestSource().parseMetaSnapshot([]byte(metaTestResponse)); len(problems) != 0 {
		t.Fatalf("exact bounds: problems = %q, want none", problems)
	}
	for _, test := range []struct {
		name      string
		configure func(*metaSource)
		response  []byte
		want      string
	}{
		{name: "IPv4 registrations", configure: func(source *metaSource) { source.minRoutes++ }, want: "IPv4 registrations"},
		{name: "IPv6 registrations", configure: func(source *metaSource) { source.minRoutes6++ }, want: "IPv6 registrations"},
		{name: "IPv4 prefixes", configure: func(source *metaSource) { source.minPrefixes++ }, want: "collapsed IPv4 prefixes"},
		{name: "IPv6 prefixes", configure: func(source *metaSource) { source.minPrefixes6++ }, want: "collapsed IPv6 prefixes"},
		{name: "IPv4 shrink", configure: func(source *metaSource) { source.minCoverage++ }, want: "suspicious shrink"},
		{name: "IPv4 growth", configure: func(source *metaSource) { source.maxCoverage-- }, want: "IPv4 coverage 84480 exceeds"},
		{name: "IPv6 growth", configure: func(source *metaSource) { source.maxCoverage48-- }, want: "IPv6 coverage"},
		{
			name: "anchor dropped",
			configure: func(source *metaSource) {
				source.anchors = append(slices.Clone(source.anchors), netip.MustParsePrefix("102.132.96.0/20"))
			},
			want: "102.132.96.0/20, where WhatsApp's chat edge resolves, is no longer covered",
		},
		{
			name: "anchor only partly covered",
			configure: func(source *metaSource) {
				source.anchors = []netip.Prefix{netip.MustParsePrefix("31.13.0.0/16")}
			},
			want: "31.13.0.0/16, where WhatsApp's chat edge resolves",
		},
		{
			name: "broad IPv4 registration",
			response: metaTestAnswer(append(metaTestObjects(), `route:          57.0.0.0/11
origin:         AS32934
mnt-by:         MAINT-AS32934
source:         RADB`)...),
			want: "57.0.0.0/11 is broader than /12",
		},
		{
			name: "broad IPv6 registration",
			response: metaTestAnswer(append(metaTestObjects(), `route6:         2a03::/23
origin:         AS32934
mnt-by:         MAINT-AS32934
source:         RADB`)...),
			want: "2a03::/23 is broader than",
		},
		{name: "empty answer", response: []byte("%  No entries found for the selected source(s).\n\n\n"), want: `no objects, server said "No entries found for the selected source(s)."`},
		{name: "reformatted answer", response: []byte(strings.ReplaceAll(metaTestResponse, "mnt-by:", "maintainer:")), want: "IPv4 registrations"},
	} {
		source := metaTestSource()
		if test.configure != nil {
			test.configure(source)
		}
		response := []byte(metaTestResponse)
		if test.response != nil {
			response = test.response
		}
		snapshot, _, problems := source.parseMetaSnapshot(response)
		if snapshot != nil || !slices.ContainsFunc(problems, func(problem string) bool { return strings.Contains(problem, test.want) }) {
			t.Errorf("%s: snapshot = %v, problems = %q, want a problem containing %q", test.name, snapshot != nil, problems, test.want)
		}
	}
}

// One query line, the answer read to the server's close, and a bound on its
// size.
func TestWhoisQuery(t *testing.T) {
	serve := func(response string) (string, <-chan string) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close() })
		queries := make(chan string, 1)
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			query, _ := bufio.NewReader(conn).ReadString('\n')
			queries <- query
			io.WriteString(conn, response)
		}()
		return listener.Addr().String(), queries
	}

	server, queries := serve(metaTestResponse)
	data, err := fetchWhois(server, "-i origin AS32934", 5*time.Second, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if query := <-queries; query != "-i origin AS32934\r\n" {
		t.Fatalf("query = %q, want the whois query line", query)
	}
	if string(data) != metaTestResponse {
		t.Fatal("response differs from what the server sent")
	}

	server, _ = serve(metaTestResponse)
	if _, err := whoisQuery(server, "-i origin AS32934", 5*time.Second, len(metaTestResponse)-1); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized response error = %v, want a size error", err)
	}

	for _, test := range []struct {
		data     string
		complete bool
	}{
		{data: "", complete: true},
		{data: "%  No entries found.\n\n", complete: true},
		{data: "route: 157.240.0.0/16\nsource: RADB\n\n", complete: true},
		{data: "route: 157.240.0.0/16\r\nsource: RADB\r\n\r\n", complete: true},
		{data: "route: 157.240.0.0/16\nsource: RADB\n", complete: false},
		{data: "route: 157.240.0.0/16\nsour", complete: false},
	} {
		if whoisComplete([]byte(test.data)) != test.complete {
			t.Errorf("complete(%q) = %v, want %v", test.data, !test.complete, test.complete)
		}
	}
}

// The checked-in table is exactly what the generator writes for its own
// provenance, meets every bound of the live source, and is collapsed.
func TestCheckedInMetaSnapshot(t *testing.T) {
	generated, err := os.ReadFile(filepath.Join("..", metaOutputFile))
	if err != nil {
		t.Fatal(err)
	}
	provenance := regexp.MustCompile(`(?m)^//   - name=radb server=whois\.radb\.net:43 query="-i origin AS32934" accepted_v4=([0-9]+) accepted_v6=([0-9]+) accepted_sha256=([0-9a-f]{64}) prefixes_v4=([0-9]+) prefixes_v6=([0-9]+) disposition=accepted_allow$`)
	records := provenance.FindAllSubmatch(generated, -1)
	if len(records) != 1 {
		t.Fatalf("provenance records = %d, want 1", len(records))
	}
	record := records[0]
	number := func(value []byte) int {
		n, err := strconv.Atoi(string(value))
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	snapshot := &metaSnapshot{
		routeCount:  number(record[1]),
		route6Count: number(record[2]),
	}
	if _, err := hex.Decode(snapshot.routesSha256[:], record[3]); err != nil {
		t.Fatal(err)
	}
	for _, match := range regexp.MustCompile(`(?m)^\tnetip\.MustParsePrefix\("([^"]+)"\),$`).FindAllSubmatch(generated, -1) {
		snapshot.prefixes = append(snapshot.prefixes, netip.MustParsePrefix(string(match[1])))
	}
	prefixCount := slices.IndexFunc(snapshot.prefixes, func(prefix netip.Prefix) bool { return prefix.Addr().Is6() })
	if prefixCount != number(record[4]) || len(snapshot.prefixes)-prefixCount != number(record[5]) {
		t.Fatalf("table holds %d IPv4 / %d IPv6 prefixes, provenance says %s / %s", prefixCount, len(snapshot.prefixes)-prefixCount, record[4], record[5])
	}
	if collapsed := collapsePrefixes(snapshot.prefixes); !slices.Equal(collapsed, snapshot.prefixes) {
		t.Fatalf("table is not collapsed and sorted: %v, collapsed %v", snapshot.prefixes, collapsed)
	}
	if problems := metaRadb.validate(snapshot); len(problems) != 0 {
		t.Fatalf("checked-in table fails the live bounds: %q", problems)
	}
	rendered, err := format.Source(metaRadb.emitMeta(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rendered, generated) {
		t.Fatalf("%s is not what the generator writes for its provenance (hand edited?); regenerate with go generate ./...", metaOutputFile)
	}
}
