package connect

import (
	"encoding/base64"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// TXT over DoH (RFC 8484 wire format), for the extender bootstrap (E3, C5).
//
// A TXT record is one or more character strings of at most 255 bytes, and a
// value longer than that is published split; the resolver joins the strings
// of each record and keeps records apart. The bootstrap depends on both: a
// signed record is well over 255 bytes, and two extenders' records must not
// run together.

// writeDohTxtWire answers a wire-format TXT query with one record per entry
// of records, each entry the record's character strings.
func writeDohTxtWire(w http.ResponseWriter, r *http.Request, records [][]string, ttl uint32) {
	raw, err := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var p dnsmessage.Parser
	header, err := p.Start(raw)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	q, err := p.Question()
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	h := dnsmessage.Header{ID: header.ID, Response: true, RecursionAvailable: true}
	b := dnsmessage.NewBuilder(nil, h)
	b.StartQuestions()
	b.Question(q)
	b.StartAnswers()
	if q.Type == dnsmessage.TypeTXT {
		rh := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: ttl}
		for _, strs := range records {
			b.TXTResource(rh, dnsmessage.TXTResource{TXT: strs})
		}
	}
	resp, err := b.Finish()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/dns-message")
	w.Write(resp)
}

func newTxtDohSettings(t *testing.T, serverUrl string) *DohSettings {
	t.Helper()
	settings := DefaultDohSettings()
	settings.RequestTimeout = 5 * time.Second
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	setRemoteDohUrls(settings, 4, serverUrl)
	return settings
}

func TestDohQueryTxtJoinsTheStringsOfEachRecord(t *testing.T) {
	ctx := t.Context()

	long := strings.Repeat("x", 255) + strings.Repeat("y", 45)
	server := newFamilyHttptestServer(t, 4, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeDohTxtWire(w, r, [][]string{
			{"short"},
			// one record published as two strings, which is how a value over
			// 255 bytes goes on the wire
			{long[:255], long[255:]},
		}, 60)
	}))
	defer server.Close()

	txts := DohQueryTxt(ctx, newTxtDohSettings(t, server.URL), "extender.example")
	slices.Sort(txts)
	want := []string{"short", long}
	slices.Sort(want)
	if !slices.Equal(txts, want) {
		t.Fatalf("txts = %q, want the two records with their strings joined", txts)
	}
}

// A name with no TXT answer yields nothing rather than an error value, since
// the bootstrap falls through to the address answers either way.
func TestDohQueryTxtEmptyAnswer(t *testing.T) {
	ctx := t.Context()

	server := newFamilyHttptestServer(t, 4, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeDohTxtWire(w, r, nil, 60)
	}))
	defer server.Close()

	if txts := DohQueryTxt(ctx, newTxtDohSettings(t, server.URL), "extender.example"); len(txts) != 0 {
		t.Fatalf("txts = %q, want none", txts)
	}
}
