package connect

// The caps the mux wires into its tunnel DoH resolver apply only while the
// tunnel DoH is warm (DnsUpgradeSettings.WarmAttemptTimeout): a cold tunnel,
// still forming, keeps the whole budget for an attempt and the dialer's own
// timeout for a dial.

import (
	"context"
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// A dial through a stalled exit ends at the warm dial cap, 5s; while cold no
// cap applies, and after the warm lease passes the tunnel is cold again.
func TestDnsWarmDialCapEndsAStalledDialAndSparesAColdOne(t *testing.T) {
	dnsFailoverSynctest(t, func(t *testing.T) {
		servers := newDnsFailoverServers(t)
		servers.ride(dnsFailoverServerA, dnsFailoverHealthyExit)
		harness := newDnsFailoverMux(t, servers, []string{dnsFailoverServerA}, nil, nil)
		defer harness.close()
		settings := harness.cache.settings
		if dialCap, requestCap := settings.DohDialCap(), settings.DohRequestCap(); dialCap != 0 || requestCap != 0 {
			t.Fatalf("cold caps dial %v request %v, want none", dialCap, requestCap)
		}
		harness.warm(0x0901)
		if dialCap, requestCap := settings.DohDialCap(), settings.DohRequestCap(); dialCap != 5*time.Second || requestCap != 4*time.Second {
			t.Fatalf("warm caps dial %v request %v, want 5s and 4s", dialCap, requestCap)
		}

		stalledDial := func(ctx context.Context, network string, address string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		dial := dohCappedDialContext(settings.DohDialCap, stalledDial)
		startTime := time.Now()
		_, err := dial(context.Background(), "tcp", "192.0.2.11:443")
		if elapsed := time.Since(startTime); elapsed != 5*time.Second || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("warm stalled dial ended after %v with %v, want 5s and a deadline", elapsed, err)
		}

		time.Sleep(31 * time.Second)
		if dialCap, requestCap := settings.DohDialCap(), settings.DohRequestCap(); dialCap != 0 || requestCap != 0 {
			t.Fatalf("caps after the warm lease dial %v request %v, want none", dialCap, requestCap)
		}
		coldCtx, coldCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer coldCancel()
		startTime = time.Now()
		dial(coldCtx, "tcp", "192.0.2.11:443")
		if elapsed := time.Since(startTime); elapsed != 20*time.Second {
			t.Fatalf("cold stalled dial ended after %v, want the dialer's own 20s", elapsed)
		}
	})
}

// A fan-out the warm cap ends demotes the servers it waited on before the
// mux retries, so the retry asks the other servers; a server that answers
// keeps its place.
func TestDnsCappedFanOutDemotesItsStalledServers(t *testing.T) {
	dnsFailoverSynctest(t, func(t *testing.T) {
		servers := newDnsFailoverServers(t)
		servers.ride(dnsFailoverServerA, dnsFailoverStalledExit)
		servers.ride(dnsFailoverServerB, dnsFailoverStalledExit)
		servers.ride(dnsFailoverServerC, dnsFailoverHealthyExit)
		servers.ride(dnsFailoverServerD, dnsFailoverHealthyExit)
		harness := newDnsFailoverMux(t, servers, []string{dnsFailoverServerA, dnsFailoverServerB, dnsFailoverServerC, dnsFailoverServerD}, nil, nil)
		defer harness.close()
		harness.order(dnsFailoverServerA, dnsFailoverServerB, dnsFailoverServerC, dnsFailoverServerD)
		harness.warm(0x0951)
		servers.stall(dnsFailoverStalledExit)
		harness.send("demote.example", dnsmessage.TypeA, 0x0952)
		time.Sleep(5 * time.Second)
		synctest.Wait()

		stats := harness.cache.remoteClient.stats
		now := time.Now()
		for _, dohUrl := range []string{dnsFailoverServerA, dnsFailoverServerB} {
			if !stats.stalled(dohUrl, now) {
				t.Errorf("%s was waited on to the cap but is not demoted", dohUrl)
			}
		}
		for _, dohUrl := range []string{dnsFailoverServerC, dnsFailoverServerD} {
			if stats.stalled(dohUrl, now) {
				t.Errorf("%s is demoted but never stalled", dohUrl)
			}
		}
		order := stats.order([]string{dnsFailoverServerA, dnsFailoverServerB, dnsFailoverServerC, dnsFailoverServerD})
		if order[2] != dnsFailoverServerA && order[2] != dnsFailoverServerB || order[3] != dnsFailoverServerA && order[3] != dnsFailoverServerB {
			t.Fatalf("order %v, want the stalled servers last", order)
		}
	})
}
