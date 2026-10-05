package main

// The Meta prefix snapshot behind the WhatsApp exception
// (ip_security_messaging.go), refreshed with the CFAA tables so it does not go
// stale between hand edits.
//
// The source is the one the snapshot always cited: the route and route6
// objects for origin AS32934 in RADb, the registry Meta documents for its
// address space, queried over whois (`whois -h whois.radb.net -- '-i origin
// AS32934'`). Only the objects Meta maintains itself are kept (RADB objects of
// MAINT-AS32934 and the RIPE objects of Meta's RIPE maintainers), as in the
// hand snapshot this replaced: third parties register origin AS32934 too (an
// ISP-hosted cache), and RADb also answers with RPKI-to-IRR conversions, which
// follow ROAs rather than Meta's registrations. The kept registrations are
// collapsed to the fewest covering prefixes, sorted IPv4 then IPv6, so the
// output depends only on the set of registrations and never on the order or
// the volatile attributes of the response.
//
// The table widens what provider DPI admits, so a refresh must be
// conservative in both directions. A dead, truncated or reformatted answer
// shrinks it (floors on the registrations and the collapsed prefixes, the
// IPv4 coverage, and anchors: the prefixes WhatsApp's chat edge resolves
// into must stay covered); a corrupted one widens it (a registration broader
// than a /12 or a /24, or coverage over the caps). Either aborts the build
// instead of writing the table.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// metaOutputFile is the generated table. Like outputFile, the generator only
// ever owns this data file; the exception's logic is the hand-written
// ip_security_messaging.go.
const metaOutputFile = "ip_security_messaging_meta.go"

// metaSource is where the snapshot comes from and the bounds a refreshed
// snapshot must meet before it is written.
type metaSource struct {
	name   string
	server string // whois host:port
	query  string
	origin string
	// the objects Meta maintains itself: source -> accepted mnt-by values
	sourceMaintainers map[string][]string
	// unique accepted registrations per family
	minRoutes  int
	minRoutes6 int
	// collapsed prefixes per family
	minPrefixes  int
	minPrefixes6 int
	// IPv4 addresses covered; IPv6 coverage in /48s
	minCoverage   uint64
	maxCoverage   uint64
	maxCoverage48 uint64
	// a registration broader than this is corruption, not Meta address space
	minBits  int
	minBits6 int
	// must stay covered: where WhatsApp's chat edge resolves
	anchors []netip.Prefix
	// bounds memory if the server misbehaves
	maxResponseBytes int
}

// metaRadb was calibrated against live data (2026-10-05): 433 IPv4 and 648
// IPv6 registrations collapsing to 23 and 5 prefixes, 531968 IPv4 addresses,
// broadest registrations a /14 and a /32. Floors sit well under the observed
// volume and caps well over it, so normal churn never trips them, but a dead,
// truncated, reformatted or poisoned answer does. The anchors are where
// g.whatsapp.net and web.whatsapp.com resolved (157.240.0.0/16 and
// 2a03:2880::/32) plus the edge range 31.13.64.0/18.
var metaRadb = metaSource{
	name:   "radb",
	server: "whois.radb.net:43",
	query:  "-i origin AS32934",
	origin: "AS32934",
	sourceMaintainers: map[string][]string{
		"RADB": {"MAINT-AS32934"},
		"RIPE": {"fb-neteng", "facebook-neteng", "meta-mnt"},
	},
	minRoutes:     200,
	minRoutes6:    300,
	minPrefixes:   12,
	minPrefixes6:  3,
	minCoverage:   200_000,
	maxCoverage:   4_194_304,
	maxCoverage48: 1_048_576,
	minBits:       12,
	minBits6:      24,
	anchors: []netip.Prefix{
		netip.MustParsePrefix("31.13.64.0/18"),
		netip.MustParsePrefix("157.240.0.0/16"),
		netip.MustParsePrefix("2a03:2880::/32"),
	},
	maxResponseBytes: 16 << 20,
}

// rpslObject is one registry object, keeping the attributes the snapshot
// filters on. Attribute names are lower case; values have their end-of-line
// comments removed.
type rpslObject struct {
	class      string
	key        string
	attributes map[string][]string
}

// metaSnapshot is a validated refresh, ready to emit.
type metaSnapshot struct {
	// unique accepted registrations per family
	routeCount  int
	route6Count int
	// SHA-256 of the sorted unique accepted registrations, one per line
	routesSha256 [32]byte
	prefixes     []netip.Prefix
}

// metaReport counts what a response held, for the build log.
type metaReport struct {
	objects            int
	routes             int
	route6s            int
	accepted           int
	rejectedSource     int
	rejectedMaintainer int
	rejectedOrigin     int
	rejectedBad        int
}

func (self metaReport) String() string {
	return fmt.Sprintf("objects=%d route=%d route6=%d accepted=%d rejected: source=%d maintainer=%d origin=%d bad=%d",
		self.objects, self.routes, self.route6s, self.accepted,
		self.rejectedSource, self.rejectedMaintainer, self.rejectedOrigin, self.rejectedBad)
}

// Queries the whois server and returns the complete response. A response
// that does not end at an object boundary was cut short and is retried like
// a failed connection.
func fetchWhois(server string, query string, timeout time.Duration, maxBytes int) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		data, err := whoisQuery(server, query, timeout, maxBytes)
		if err != nil {
			lastErr = err
			continue
		}
		if !whoisComplete(data) {
			lastErr = fmt.Errorf("response of %d bytes ends inside an object (truncated)", len(data))
			continue
		}
		return data, nil
	}
	return nil, lastErr
}

// whoisComplete reports whether a response ends at an object boundary (a
// blank line). An empty response has no object to cut short.
func whoisComplete(data []byte) bool {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	return len(data) == 0 || bytes.HasSuffix(data, []byte("\n\n"))
}

// One whois exchange: the query line, then the answer until the server closes
// the connection.
func whoisQuery(server string, query string, timeout time.Duration, maxBytes int) ([]byte, error) {
	conn, err := net.DialTimeout("tcp", server, timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	if _, err := io.WriteString(conn, query+"\r\n"); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(conn, int64(maxBytes)+1))
	if err != nil {
		return nil, err
	}
	if maxBytes < len(data) {
		return nil, fmt.Errorf("response exceeds %d bytes", maxBytes)
	}
	return data, nil
}

// parseRpsl splits a whois response into its objects. Lines starting with %
// are server messages (returned so an empty answer can say why), # lines are
// comments, and a line starting with white space or + continues the previous
// attribute. The first attribute of an object names its class.
func parseRpsl(data []byte) (objects []rpslObject, messages []string) {
	var object *rpslObject
	var lastName string
	flush := func() {
		if object != nil {
			objects = append(objects, *object)
		}
		object = nil
		lastName = ""
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case strings.TrimSpace(line) == "":
			flush()
		case strings.HasPrefix(line, "%"):
			messages = append(messages, strings.TrimSpace(strings.TrimPrefix(line, "%")))
		case strings.HasPrefix(line, "#"):
		case line[0] == ' ' || line[0] == '\t' || line[0] == '+':
			if object != nil && lastName != "" {
				values := object.attributes[lastName]
				if continued := rpslValue(line[1:]); continued != "" {
					values[len(values)-1] = strings.TrimSpace(values[len(values)-1] + " " + continued)
				}
			}
		default:
			i := strings.IndexByte(line, ':')
			if i <= 0 {
				continue
			}
			name := strings.ToLower(strings.TrimSpace(line[:i]))
			value := rpslValue(line[i+1:])
			if object == nil {
				object = &rpslObject{
					class:      name,
					key:        value,
					attributes: map[string][]string{},
				}
			}
			object.attributes[name] = append(object.attributes[name], value)
			lastName = name
		}
	}
	flush()
	return
}

// rpslValue drops an end-of-line comment and surrounding white space.
func rpslValue(value string) string {
	if i := strings.IndexByte(value, '#'); i >= 0 {
		value = value[:i]
	}
	return strings.TrimSpace(value)
}

// rpslList splits a list attribute's values, which may also be comma
// separated within one line.
func rpslList(values []string) []string {
	var items []string
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			if item = strings.TrimSpace(item); item != "" {
				items = append(items, item)
			}
		}
	}
	return items
}

// Keeps the route and route6 objects Meta maintains itself. A registration
// broader than the source allows is returned as a problem, since it can only
// be corruption, never quietly dropped.
func (self *metaSource) acceptObjects(objects []rpslObject) (routes []netip.Prefix, report metaReport, problems []string) {
	report.objects = len(objects)
	for _, object := range objects {
		var family int
		switch object.class {
		case "route":
			family = 4
			report.routes++
		case "route6":
			family = 6
			report.route6s++
		default:
			continue
		}
		sources := object.attributes["source"]
		if len(sources) != 1 {
			report.rejectedSource++
			continue
		}
		maintainers, ok := self.sourceMaintainers[strings.ToUpper(sources[0])]
		if !ok {
			report.rejectedSource++
			continue
		}
		maintained := false
		for _, maintainer := range rpslList(object.attributes["mnt-by"]) {
			for _, accepted := range maintainers {
				if strings.EqualFold(maintainer, accepted) {
					maintained = true
				}
			}
		}
		if !maintained {
			report.rejectedMaintainer++
			continue
		}
		if origins := object.attributes["origin"]; len(origins) != 1 || !strings.EqualFold(origins[0], self.origin) {
			report.rejectedOrigin++
			continue
		}
		prefix, err := netip.ParsePrefix(object.key)
		if err != nil || prefix != prefix.Masked() ||
			(family == 4) != prefix.Addr().Is4() || prefix.Addr().Is4In6() {
			report.rejectedBad++
			continue
		}
		if (family == 4 && prefix.Bits() < self.minBits) || (family == 6 && prefix.Bits() < self.minBits6) {
			problems = append(problems, fmt.Sprintf("%s: registration %s is broader than /%d (IPv4) or /%d (IPv6) — possible poisoned answer",
				self.name, prefix, self.minBits, self.minBits6))
			continue
		}
		routes = append(routes, prefix)
		report.accepted++
	}
	return
}

// collapsePrefixes returns the fewest prefixes covering exactly the union of
// prefixes, sorted IPv4 then IPv6 by address: duplicates and contained
// prefixes are dropped, and overlapping or adjacent ones merged.
func collapsePrefixes(prefixes []netip.Prefix) []netip.Prefix {
	sorted := slices.Clone(prefixes)
	slices.SortFunc(sorted, netip.Prefix.Compare)
	var collapsed []netip.Prefix
	var lo, hi netip.Addr
	for i, prefix := range sorted {
		first, last := prefix.Addr(), prefixLast(prefix)
		if 0 < i && first.BitLen() == hi.BitLen() && (first.Compare(hi) <= 0 || first == hi.Next()) {
			if hi.Compare(last) < 0 {
				hi = last
			}
			continue
		}
		if 0 < i {
			collapsed = append(collapsed, rangePrefixes(lo, hi)...)
		}
		lo, hi = first, last
	}
	if 0 < len(sorted) {
		collapsed = append(collapsed, rangePrefixes(lo, hi)...)
	}
	return collapsed
}

// rangePrefixes splits the address range lo..hi of one family into the
// fewest prefixes, each the widest aligned block that starts at the next
// uncovered address.
func rangePrefixes(lo, hi netip.Addr) []netip.Prefix {
	var prefixes []netip.Prefix
	for {
		bits := lo.BitLen()
		for 0 < bits {
			wider := netip.PrefixFrom(lo, bits-1).Masked()
			if wider.Addr() != lo || hi.Compare(prefixLast(wider)) < 0 {
				break
			}
			bits--
		}
		prefix := netip.PrefixFrom(lo, bits)
		prefixes = append(prefixes, prefix)
		last := prefixLast(prefix)
		if hi.Compare(last) <= 0 {
			return prefixes
		}
		lo = last.Next()
	}
}

// prefixLast returns the last address of a prefix of either family.
func prefixLast(prefix netip.Prefix) netip.Addr {
	prefix = prefix.Masked()
	if prefix.Addr().Is4() {
		a := prefix.Addr().As4()
		for i := prefix.Bits(); i < 32; i++ {
			a[i/8] |= 1 << (7 - uint(i%8))
		}
		return netip.AddrFrom4(a)
	}
	_, hi := prefixBounds6(prefix)
	return hi
}

// Coverage of collapsed prefixes: IPv4 addresses, and IPv6 in /48s (a
// narrower prefix counts as one).
func prefixCoverage(prefixes []netip.Prefix) (coverage uint64, coverage48 uint64) {
	for _, prefix := range prefixes {
		if prefix.Addr().Is4() {
			coverage += uint64(1) << (32 - prefix.Bits())
		} else if prefix.Bits() < 48 {
			coverage48 += uint64(1) << (48 - prefix.Bits())
		} else {
			coverage48++
		}
	}
	return
}

// newMetaSnapshot collapses the accepted registrations and checks every
// bound; any problem means the snapshot must not be written.
func (self *metaSource) newMetaSnapshot(routes []netip.Prefix) (*metaSnapshot, []string) {
	unique := slices.Clone(routes)
	slices.SortFunc(unique, netip.Prefix.Compare)
	unique = slices.Compact(unique)
	snapshot := &metaSnapshot{
		prefixes: collapsePrefixes(unique),
	}
	digest := sha256.New()
	for _, prefix := range unique {
		if prefix.Addr().Is4() {
			snapshot.routeCount++
		} else {
			snapshot.route6Count++
		}
		fmt.Fprintf(digest, "%s\n", prefix)
	}
	copy(snapshot.routesSha256[:], digest.Sum(nil))
	return snapshot, self.validate(snapshot)
}

// validate returns every bound the snapshot misses; none means it may be
// written.
func (self *metaSource) validate(snapshot *metaSnapshot) []string {
	var problems []string
	if snapshot.routeCount < self.minRoutes {
		problems = append(problems, fmt.Sprintf("%s: only %d IPv4 registrations (min %d) — answer may be truncated or reformatted",
			self.name, snapshot.routeCount, self.minRoutes))
	}
	if snapshot.route6Count < self.minRoutes6 {
		problems = append(problems, fmt.Sprintf("%s: only %d IPv6 registrations (min %d) — answer may be truncated or reformatted",
			self.name, snapshot.route6Count, self.minRoutes6))
	}
	prefixCount, prefix6Count := 0, 0
	for _, prefix := range snapshot.prefixes {
		if prefix.Addr().Is4() {
			prefixCount++
		} else {
			prefix6Count++
		}
	}
	if prefixCount < self.minPrefixes {
		problems = append(problems, fmt.Sprintf("%s: only %d collapsed IPv4 prefixes (min %d)", self.name, prefixCount, self.minPrefixes))
	}
	if prefix6Count < self.minPrefixes6 {
		problems = append(problems, fmt.Sprintf("%s: only %d collapsed IPv6 prefixes (min %d)", self.name, prefix6Count, self.minPrefixes6))
	}
	coverage, coverage48 := prefixCoverage(snapshot.prefixes)
	if coverage < self.minCoverage {
		problems = append(problems, fmt.Sprintf("%s: IPv4 coverage %d is under the floor %d — suspicious shrink", self.name, coverage, self.minCoverage))
	}
	if self.maxCoverage < coverage {
		problems = append(problems, fmt.Sprintf("%s: IPv4 coverage %d exceeds max %d — possible poisoned answer", self.name, coverage, self.maxCoverage))
	}
	if self.maxCoverage48 < coverage48 {
		problems = append(problems, fmt.Sprintf("%s: IPv6 coverage of %d /48s exceeds max %d — possible poisoned answer", self.name, coverage48, self.maxCoverage48))
	}
	for _, anchor := range self.anchors {
		covered := false
		for _, prefix := range snapshot.prefixes {
			if prefix.Bits() <= anchor.Bits() && prefix.Contains(anchor.Addr()) {
				covered = true
			}
		}
		if !covered {
			problems = append(problems, fmt.Sprintf("%s: %s, where WhatsApp's chat edge resolves, is no longer covered — suspicious shrink", self.name, anchor))
		}
	}
	return problems
}

// fetchMetaSnapshot queries the source and returns a validated snapshot, or
// the problems that keep it from being written.
func (self *metaSource) fetchMetaSnapshot(timeout time.Duration) (*metaSnapshot, metaReport, []string) {
	data, err := fetchWhois(self.server, self.query, timeout, self.maxResponseBytes)
	if err != nil {
		return nil, metaReport{}, []string{fmt.Sprintf("%s: whois %s %q: %v", self.name, self.server, self.query, err)}
	}
	return self.parseMetaSnapshot(data)
}

// parseMetaSnapshot is fetchMetaSnapshot for a response already read.
func (self *metaSource) parseMetaSnapshot(data []byte) (*metaSnapshot, metaReport, []string) {
	objects, messages := parseRpsl(data)
	routes, report, problems := self.acceptObjects(objects)
	if len(objects) == 0 && 0 < len(messages) {
		problems = append(problems, fmt.Sprintf("%s: no objects, server said %q", self.name, messages[0]))
	}
	snapshot, validateProblems := self.newMetaSnapshot(routes)
	problems = append(problems, validateProblems...)
	if 0 < len(problems) {
		return nil, report, problems
	}
	return snapshot, report, nil
}

const metaHeader = `// Code generated by security/main.go; DO NOT EDIT.
//
// Meta prefix snapshot for the WhatsApp exception (ip_security_messaging.go):
// the address space Meta registers for its own AS32934, collapsed to the
// fewest covering prefixes, sorted IPv4 then IPv6. A flow to TCP/5222 inside
// it is allowed after the BitTorrent signatures and the application
// standards, as the backstop of the WhatsApp Noise detector.
//
// Regenerate with:  go generate ./...   (or: cd security && go run .)
//
// Every release build refreshes it together with the CFAA tables, so like
// them it is identified by SecurityPolicyHash and is not part of
// SecurityPolicyRulesGeneration (ip_provider_diagnostics.go).
//
// Source: the route and route6 objects for origin AS32934 in RADb, the
// registry Meta documents for its address space (whois.radb.net, query
// "-i origin AS32934"), keeping only the objects Meta maintains itself: source
// RADB with mnt-by MAINT-AS32934, and source RIPE with mnt-by fb-neteng,
// facebook-neteng or meta-mnt. Objects third parties registered with origin
// AS32934 (an ISP-hosted cache) and the RPKI-to-IRR conversions are left out.
//
// Generated input provenance v1:
`

// metaPackage follows the provenance record and opens the table, IPv4 first.
const metaPackage = `
package connect

import (
	"net/netip"
)

var metaNetworkPrefixes = [...]netip.Prefix{
	// IPv4
`

// emitMeta renders the generated file. It depends only on the snapshot, so
// the same registrations always give the same bytes.
func (self *metaSource) emitMeta(snapshot *metaSnapshot) []byte {
	var b strings.Builder
	b.WriteString(metaHeader)
	prefixCount := 0
	for _, prefix := range snapshot.prefixes {
		if prefix.Addr().Is4() {
			prefixCount++
		}
	}
	fmt.Fprintf(&b, "//   - name=%s server=%s query=%q accepted_v4=%d accepted_v6=%d accepted_sha256=%x prefixes_v4=%d prefixes_v6=%d disposition=accepted_allow\n",
		self.name, self.server, self.query, snapshot.routeCount, snapshot.route6Count, snapshot.routesSha256,
		prefixCount, len(snapshot.prefixes)-prefixCount)
	b.WriteString(metaPackage)
	for i, prefix := range snapshot.prefixes {
		if i == prefixCount {
			b.WriteString("\n\t// IPv6\n")
		}
		fmt.Fprintf(&b, "\tnetip.MustParsePrefix(%q),\n", prefix.String())
	}
	b.WriteString("}\n")
	return []byte(b.String())
}
