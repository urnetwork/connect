package connect

// The DoH cache mechanisms behind the in-tunnel DNS failover: the stall
// demotion of a server's place in the launch order, the ip family a fan-out
// keeps, the ttl rewrite of a forwarded or cached raw response, and the
// caching of raw HTTPS answers. All pure: fixed clocks, synthetic responses.

import (
	"encoding/binary"
	"net/netip"
	"slices"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// A stalled server goes after every other server until its demotion passes
// or it answers again, whatever its score.
func TestDohServerStallDemotesUntilItPassesOrTheServerAnswers(t *testing.T) {
	stats := newServerStats()
	stats.stallDemotion = 30 * time.Second
	// fixed draws: a before b before c at equal scores
	stats.draw = func(dohUrl string) float64 {
		return map[string]float64{"a": 0.9, "b": 0.8, "c": 0.7}[dohUrl]
	}
	now := time.Unix(1000, 0)
	// a has the best score by far
	for range 5 {
		stats.recordAt("a", true, now)
	}
	urls := []string{"a", "b", "c"}
	if order := stats.orderAt(urls, now); !slices.Equal(order, []string{"a", "b", "c"}) {
		t.Fatalf("order %v, want a b c", order)
	}
	stats.recordStallAt("a", now)
	if order := stats.orderAt(urls, now.Add(29*time.Second)); !slices.Equal(order, []string{"b", "c", "a"}) {
		t.Fatalf("order %v inside the demotion, want b c a", order)
	}
	if order := stats.orderAt(urls, now.Add(30*time.Second)); !slices.Equal(order, []string{"a", "b", "c"}) {
		t.Fatalf("order %v once the demotion passed, want a b c", order)
	}
	stats.recordStallAt("a", now.Add(40*time.Second))
	stats.recordAt("a", true, now.Add(41*time.Second))
	if order := stats.orderAt(urls, now.Add(42*time.Second)); !slices.Equal(order, []string{"a", "b", "c"}) {
		t.Fatalf("order %v after the server answered, want a b c", order)
	}
}

// With no demotion configured a stall changes nothing.
func TestDohServerStallWithoutDemotionKeepsTheOrder(t *testing.T) {
	stats := newServerStats()
	stats.draw = func(dohUrl string) float64 {
		return map[string]float64{"a": 0.9, "b": 0.8}[dohUrl]
	}
	now := time.Unix(1000, 0)
	stats.recordStallAt("a", now)
	if order := stats.orderAt([]string{"a", "b"}, now); !slices.Equal(order, []string{"a", "b"}) {
		t.Fatalf("order %v, want a b", order)
	}
}

// A fan-out whose first servers are all of one family takes the best server
// of the other family in its last place, another operator's first; it keeps
// a lead server, and skips a demoted server of the other family.
func TestDohFanOutServersKeepsBothFamilies(t *testing.T) {
	const (
		a4 = "https://192.0.2.21/dns-query"
		b4 = "https://192.0.2.22/dns-query"
		c4 = "https://192.0.2.23/dns-query"
		a6 = "https://[2001:db8::21]/dns-query"
		b6 = "https://[2001:db8::22]/dns-query"
	)
	client := &dohClient{
		stats: newServerStats(),
		siblings: map[string]string{
			a4: a6, a6: a4,
			b4: b6, b6: b4,
		},
	}
	client.stats.stallDemotion = time.Minute
	cases := []struct {
		ordered []string
		want    []string
	}{
		// both families already: unchanged
		{ordered: []string{a6, a4, b6, b4}, want: []string{a6, a4}},
		// two ipv6 servers: the ipv4 server of another operator is preferred
		{ordered: []string{a6, b6, c4, a4}, want: []string{a6, c4}},
		// only siblings of the kept servers: the best of them
		{ordered: []string{a6, b6, a4, b4}, want: []string{a6, a4}},
		// two ipv4 servers: an ipv6 server takes the last place
		{ordered: []string{a4, b4, b6}, want: []string{a4, b6}},
		// nothing to cut
		{ordered: []string{a6, b6}, want: []string{a6, b6}},
	}
	for _, c := range cases {
		if got := client.fanOutServers(c.ordered, 2); !slices.Equal(got, c.want) {
			t.Errorf("fanOutServers(%v) = %v, want %v", c.ordered, got, c.want)
		}
	}
	// one server keeps its place; no cap launches all
	if got := client.fanOutServers([]string{a6, b6, c4}, 1); !slices.Equal(got, []string{a6}) {
		t.Errorf("fanOutServers of one = %v, want [%s]", got, a6)
	}
	if got := client.fanOutServers([]string{a6, b6, c4}, 0); !slices.Equal(got, []string{a6, b6, c4}) {
		t.Errorf("fanOutServers without a cap = %v, want all", got)
	}

	// a demoted ipv4 server is not pulled in
	client.stats.recordStall(c4)
	if got := client.fanOutServers([]string{a6, b6, c4, a4}, 2); !slices.Equal(got, []string{a6, a4}) {
		t.Errorf("with c4 demoted fanOutServers = %v, want [%s %s]", got, a6, a4)
	}
	// a lead server keeps its place
	client.leadDohUrls = map[string]bool{b6: true}
	if got := client.fanOutServers([]string{a6, b6, c4, a4}, 2); !slices.Equal(got, []string{a6, b6}) {
		t.Errorf("with a lead last fanOutServers = %v, want [%s %s]", got, a6, b6)
	}
}

// A synthetic response: one question, two answers (one compressed name),
// an authority record and an OPT record whose ttl field carries flags.
func testingDohTtlResponse(t *testing.T) []byte {
	t.Helper()
	name := dnsmessage.MustNewName("ttl.example.")
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 7, Response: true})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := b.Question(dnsmessage.Question{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}); err != nil {
		t.Fatal(err)
	}
	if err := b.StartAnswers(); err != nil {
		t.Fatal(err)
	}
	for i, ttl := range []uint32{3600, 90} {
		rh := dnsmessage.ResourceHeader{Name: name, Class: dnsmessage.ClassINET, TTL: ttl}
		if err := b.AResource(rh, dnsmessage.AResource{A: [4]byte{192, 0, 2, byte(30 + i)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.StartAuthorities(); err != nil {
		t.Fatal(err)
	}
	if err := b.NSResource(dnsmessage.ResourceHeader{Name: name, Class: dnsmessage.ClassINET, TTL: 600}, dnsmessage.NSResource{NS: dnsmessage.MustNewName("ns.ttl.example.")}); err != nil {
		t.Fatal(err)
	}
	if err := b.StartAdditionals(); err != nil {
		t.Fatal(err)
	}
	var optHeader dnsmessage.ResourceHeader
	if err := optHeader.SetEDNS0(1232, dnsmessage.RCodeSuccess, true); err != nil {
		t.Fatal(err)
	}
	if err := b.OPTResource(optHeader, dnsmessage.OPTResource{}); err != nil {
		t.Fatal(err)
	}
	response, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// Every record's ttl is rewritten in place, the OPT record's flags are not,
// and the input is left as it was.
func TestDnsResponseWithTtlsRewritesRecordsButNotOpt(t *testing.T) {
	response := testingDohTtlResponse(t)
	original := slices.Clone(response)
	rewritten, ok := dnsResponseWithTtls(response, func(ttl uint32) uint32 {
		return min(ttl, 300)
	})
	if !ok {
		t.Fatal("a well formed response did not walk")
	}
	if !slices.Equal(response, original) {
		t.Fatal("the input response was changed")
	}
	var message dnsmessage.Message
	if err := message.Unpack(rewritten); err != nil {
		t.Fatal(err)
	}
	ttls := []uint32{}
	for _, answer := range message.Answers {
		ttls = append(ttls, answer.Header.TTL)
	}
	for _, authority := range message.Authorities {
		ttls = append(ttls, authority.Header.TTL)
	}
	if !slices.Equal(ttls, []uint32{300, 90, 300}) {
		t.Fatalf("ttls %v, want [300 90 300]", ttls)
	}
	if len(message.Additionals) != 1 || !message.Additionals[0].Header.DNSSECAllowed() {
		t.Fatal("the OPT record's flags were rewritten")
	}
	if len(rewritten) != len(response) {
		t.Fatalf("length %d, want %d: the rewrite must not repack", len(rewritten), len(response))
	}

	// a truncated message does not walk
	if _, ok := dnsResponseWithTtls(response[:len(response)-3], func(ttl uint32) uint32 { return ttl }); ok {
		t.Fatal("a truncated response walked")
	}
	short := slices.Clone(response)
	binary.BigEndian.PutUint16(short[6:8], 9)
	if _, ok := dnsResponseWithTtls(short, func(ttl uint32) uint32 { return ttl }); ok {
		t.Fatal("a response claiming more records than it holds walked")
	}
}

// A raw HTTPS answer is cached until its least ttl, served with the ttl
// lowered by its age, then stale-usable within the stale bound; a NODATA is a
// miss, never stale; a type the cache does not keep is never cached.
func TestDohRawResponseCacheFollowsTheAddressRules(t *testing.T) {
	settings := DefaultDohSettings()
	settings.Log = NewNoopLogger()
	cache := NewDohCache(settings)
	defer cache.Close()
	now := time.Unix(1000, 0)
	question := dnsmessage.Question{Name: dnsmessage.MustNewName("raw.example."), Type: dnsTypeHttps, Class: dnsmessage.ClassINET}
	positive := dnsFailoverResponse(t, 0, question, dnsFailoverAnswer{httpsHint: netip.MustParseAddr("192.0.2.40"), ttl: 120})
	cache.storeForward(dnsmessage.TypeHTTPS, "raw.example", positive, "https://192.0.2.41/dns-query", now)

	response, ok := cache.cachedForward(dnsmessage.TypeHTTPS, "raw.example", now.Add(20*time.Second))
	if !ok {
		t.Fatal("the cached HTTPS answer was not served")
	}
	var message dnsmessage.Message
	if err := message.Unpack(response); err != nil {
		t.Fatal(err)
	}
	if len(message.Answers) != 1 || message.Answers[0].Header.TTL != 100 {
		t.Fatalf("cached answer %+v, want one record at ttl 100", message.Answers)
	}
	if _, ok := cache.cachedForward(dnsmessage.TypeHTTPS, "raw.example", now.Add(121*time.Second)); ok {
		t.Fatal("an expired HTTPS answer was served fresh")
	}
	if _, ok := cache.staleForward(dnsmessage.TypeHTTPS, "raw.example", now.Add(121*time.Second)); !ok {
		t.Fatal("an expired HTTPS answer was not stale-usable")
	}
	if _, ok := cache.staleForward(dnsmessage.TypeHTTPS, "raw.example", now.Add(120*time.Second+dohStaleServeBound+time.Second)); ok {
		t.Fatal("an HTTPS answer past the stale bound was stale-usable")
	}

	nodata := dnsFailoverResponse(t, 0, question, dnsFailoverAnswer{ttl: 120})
	cache.storeForward(dnsmessage.TypeHTTPS, "nodata.example", nodata, "https://192.0.2.41/dns-query", now)
	if _, ok := cache.cachedForward(dnsmessage.TypeHTTPS, "nodata.example", now.Add(time.Minute)); !ok {
		t.Fatal("a NODATA answer was not cached as a miss")
	}
	if _, ok := cache.staleForward(dnsmessage.TypeHTTPS, "nodata.example", now.Add(time.Hour)); ok {
		t.Fatal("a miss was stale-usable")
	}

	txt := dnsFailoverResponse(t, 0, dnsmessage.Question{Name: question.Name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET}, dnsFailoverAnswer{ttl: 120})
	cache.storeForward(dnsmessage.TypeTXT, "raw.example", txt, "https://192.0.2.41/dns-query", now)
	if _, ok := cache.cachedForward(dnsmessage.TypeTXT, "raw.example", now); ok {
		t.Fatal("a TXT answer was cached")
	}
}
